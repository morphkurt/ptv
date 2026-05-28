package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"

	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	awsevents "github.com/aws/aws-lambda-go/events"
	"github.com/aws/aws-lambda-go/lambda"
)

var (
	dbClient  *dynamodb.Client
	dbTable   string
)

func initDynamo(ctx context.Context) {
	dbTable = os.Getenv("DYNAMODB_TABLE")
	if dbTable == "" {
		return
	}
	cfg, err := awsconfig.LoadDefaultConfig(ctx)
	if err != nil {
		log.Printf("dynamodb init: %v", err)
		return
	}
	dbClient = dynamodb.NewFromConfig(cfg)
}

// universalHandler accepts both API Gateway v2 and EventBridge Scheduled events.
// API Gateway events always have "rawPath"; EventBridge events do not.
func universalHandler(ctx context.Context, raw json.RawMessage) (interface{}, error) {
	var probe struct {
		RawPath string `json:"rawPath"`
	}
	json.Unmarshal(raw, &probe)

	if probe.RawPath == "" {
		// EventBridge scheduled event — run collector
		if dbClient == nil || dbTable == "" {
			log.Println("collector: DynamoDB not configured, skipping")
			return nil, nil
		}
		return nil, Collect(ctx, newClient(), dbClient, dbTable)
	}

	var req awsevents.APIGatewayV2HTTPRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		return nil, err
	}
	return apiHandler(ctx, req)
}

func apiHandler(ctx context.Context, req awsevents.APIGatewayV2HTTPRequest) (awsevents.APIGatewayV2HTTPResponse, error) {
	var resp awsevents.APIGatewayV2HTTPResponse
	var err error

	switch req.RawPath {
	case "/travel-times":
		resp, err = handleTravelTimes(req)
	case "/track":
		resp, err = handleTrack(req)
	case "/history":
		if dbClient == nil || dbTable == "" {
			resp = errResponse(http.StatusServiceUnavailable, "history not configured")
		} else {
			resp, err = handleHistory(req, dbClient, dbTable)
		}
	default:
		resp, err = handleTimetable(req)
	}

	if resp.Headers == nil {
		resp.Headers = map[string]string{}
	}
	resp.Headers["Access-Control-Allow-Origin"] = "*"
	return resp, err
}

func handleTimetable(req awsevents.APIGatewayV2HTTPRequest) (awsevents.APIGatewayV2HTTPResponse, error) {
	params := req.QueryStringParameters
	date := params["date"]
	realtime := params["realtime"] == "true"
	client := newClient()

	var results []JourneyResult
	var err error

	if from := params["from"]; from != "" {
		departAt := params["depart_at"]
		if departAt == "" {
			return errResponse(http.StatusBadRequest, "missing required query param: depart_at"), nil
		}
		results, err = PlanReturnJourney(client, from, departAt, date, realtime)
	} else if destination := params["destination"]; destination != "" {
		if arriveBy := params["arrive_by"]; arriveBy != "" {
			results, err = PlanJourney(client, destination, arriveBy, date, realtime)
		} else if departAt := params["depart_at"]; departAt != "" {
			results, err = PlanOutboundJourney(client, destination, departAt, date, realtime)
		} else {
			return errResponse(http.StatusBadRequest, "provide arrive_by or depart_at with destination"), nil
		}
	} else {
		return errResponse(http.StatusBadRequest, "provide destination+arrive_by, destination+depart_at, or from+depart_at"), nil
	}
	if err != nil {
		log.Printf("journey error: %v", err)
		return errResponse(http.StatusBadRequest, err.Error()), nil
	}

	body, _ := json.Marshal(results)
	return awsevents.APIGatewayV2HTTPResponse{
		StatusCode: http.StatusOK,
		Headers:    map[string]string{"Content-Type": "application/json"},
		Body:       string(body),
	}, nil
}

func handleTrack(req awsevents.APIGatewayV2HTTPRequest) (awsevents.APIGatewayV2HTTPResponse, error) {
	params := req.QueryStringParameters
	runRef := params["run_ref"]
	destination := params["destination"]
	if runRef == "" || destination == "" {
		return errResponse(http.StatusBadRequest, "missing run_ref or destination"), nil
	}

	destStops := map[string]int{
		"mentone":      stopMentone,
		"sandown_park": stopSandownPark,
		"townhall":     stopTownHall,
		"caulfield":    stopCaulfield,
	}
	stopID, ok := destStops[destination]
	if !ok {
		return errResponse(http.StatusBadRequest, "unknown destination"), nil
	}

	stops, err := newClient().GetPattern(runRef)
	if err != nil {
		log.Printf("track GetPattern %s: %v", runRef, err)
		return errResponse(http.StatusBadRequest, err.Error()), nil
	}
	if len(stops) == 0 {
		log.Printf("track: run %s returned empty pattern (stale run_ref?)", runRef)
		return errResponse(http.StatusNotFound, "run not found in today's timetable"), nil
	}

	for _, s := range stops {
		if s.StopID != stopID {
			continue
		}
		// Prefer estimated (live GPS) over scheduled; fall back gracefully if neither parses.
		timeStr, isRealtime := s.EstimatedDeparture, true
		if timeStr == "" {
			timeStr, isRealtime = s.ScheduledDeparture, false
		}
		t, err := parseUTC(timeStr)
		if err != nil {
			log.Printf("track: run %s stop %d unparseable time %q: %v", runRef, stopID, timeStr, err)
			continue
		}
		result := map[string]interface{}{
			"arrive_at": t.In(melbourneTZ).Format("15:04"),
			"realtime":  isRealtime,
		}
		body, _ := json.Marshal(result)
		return awsevents.APIGatewayV2HTTPResponse{
			StatusCode: http.StatusOK,
			Headers:    map[string]string{"Content-Type": "application/json"},
			Body:       string(body),
		}, nil
	}

	log.Printf("track: run %s (%d stops) does not serve stop %d (%s)", runRef, len(stops), stopID, destination)
	return errResponse(http.StatusNotFound, "stop not found in run pattern"), nil
}

func handleTravelTimes(req awsevents.APIGatewayV2HTTPRequest) (awsevents.APIGatewayV2HTTPResponse, error) {
	params := req.QueryStringParameters
	route := params["route"]
	date := params["date"]
	if route == "" {
		return errResponse(http.StatusBadRequest, "missing required query param: route"), nil
	}
	result, err := GetTravelTimes(newClient(), route, date)
	if err != nil {
		log.Printf("GetTravelTimes error: %v", err)
		return errResponse(http.StatusBadRequest, err.Error()), nil
	}
	body, _ := json.Marshal(result)
	return awsevents.APIGatewayV2HTTPResponse{
		StatusCode: http.StatusOK,
		Headers:    map[string]string{"Content-Type": "application/json"},
		Body:       string(body),
	}, nil
}

func errResponse(code int, msg string) awsevents.APIGatewayV2HTTPResponse {
	body, _ := json.Marshal(map[string]string{"error": msg})
	return awsevents.APIGatewayV2HTTPResponse{
		StatusCode: code,
		Headers:    map[string]string{"Content-Type": "application/json"},
		Body:       string(body),
	}
}

func newClient() *PTVClient {
	devID := os.Getenv("PTV_DEV_ID")
	apiKey := os.Getenv("PTV_API_KEY")
	if devID == "" {
		devID = "1000080"
	}
	if apiKey == "" {
		apiKey = "786416fc-aa6c-11e3-8bed-0263a9d0b8a0"
	}
	return NewPTVClient(devID, apiKey)
}

func main() {
	if os.Getenv("LAMBDA_TASK_ROOT") != "" {
		ctx := context.Background()
		initDynamo(ctx)
		lambda.Start(universalHandler)
		return
	}

	// Local CLI mode
	dest := flag.String("dest", "", "outbound destination: mentone or sandown_park")
	arrive := flag.String("arrive", "", "outbound arrive-by time HH:MM")
	from := flag.String("from", "", "return origin: mentone or sandown_park")
	depart := flag.String("depart", "", "return departure time HH:MM")
	date := flag.String("date", "", "date YYYY-MM-DD (default: today)")
	realtime := flag.Bool("realtime", false, "use live estimated departure times")
	flag.Parse()

	var results []JourneyResult
	var err error
	switch {
	case *from != "" && *depart != "":
		results, err = PlanReturnJourney(newClient(), *from, *depart, *date, *realtime)
	case *dest != "" && *arrive != "":
		results, err = PlanJourney(newClient(), *dest, *arrive, *date, *realtime)
	default:
		fmt.Fprintln(os.Stderr, "usage:")
		fmt.Fprintln(os.Stderr, "  outbound: ptv -dest mentone -arrive 17:30 [-date YYYY-MM-DD] [-realtime]")
		fmt.Fprintln(os.Stderr, "  return:   ptv -from mentone -depart 08:00 [-date YYYY-MM-DD] [-realtime]")
		os.Exit(1)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}

	out, _ := json.MarshalIndent(results, "", "  ")
	fmt.Println(string(out))
}
