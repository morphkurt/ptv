package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsevents "github.com/aws/aws-lambda-go/events"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
)

type HistoricalPoint struct {
	RunRef             string `json:"run_ref"`
	DepartScheduled    string `json:"depart_scheduled"`
	DepartActual       string `json:"depart_actual,omitempty"`
	ArriveScheduled    string `json:"arrive_scheduled"`
	ArriveActual       string `json:"arrive_actual,omitempty"`
	TravelMinScheduled int    `json:"travel_min_scheduled"`
	TravelMinActual    *int   `json:"travel_min_actual,omitempty"`
	DelayMin           int    `json:"delay_min"`
}

type HistoryResult struct {
	Route  string            `json:"route"`
	Date   string            `json:"date"`
	Trains []HistoricalPoint `json:"trains"`
}

func handleHistory(req awsevents.APIGatewayV2HTTPRequest, db *dynamodb.Client, table string) (awsevents.APIGatewayV2HTTPResponse, error) {
	route := req.QueryStringParameters["route"]
	date := req.QueryStringParameters["date"]
	if route == "" {
		return errResponse(http.StatusBadRequest, "missing required query param: route"), nil
	}
	if date == "" {
		date = time.Now().In(melbourneTZ).Format("2006-01-02")
	}

	result, err := QueryHistory(context.Background(), db, table, route, date)
	if err != nil {
		return errResponse(http.StatusInternalServerError, err.Error()), nil
	}

	body, _ := json.Marshal(result)
	return awsevents.APIGatewayV2HTTPResponse{
		StatusCode: http.StatusOK,
		Headers:    map[string]string{"Content-Type": "application/json"},
		Body:       string(body),
	}, nil
}

func QueryHistory(ctx context.Context, db *dynamodb.Client, table, route, date string) (*HistoryResult, error) {
	out, err := db.Query(ctx, &dynamodb.QueryInput{
		TableName:              aws.String(table),
		KeyConditionExpression: aws.String("pk = :pk"),
		ExpressionAttributeValues: map[string]types.AttributeValue{
			":pk": &types.AttributeValueMemberS{Value: date + "#" + route},
		},
	})
	if err != nil {
		return nil, fmt.Errorf("dynamodb query: %w", err)
	}

	// Deduplicate by run_ref — keep the record with the latest collected_at.
	seen := map[string]HistoricalPoint{}
	seenAt := map[string]string{}

	for _, item := range out.Items {
		runRef := strVal(item, "sk")
		collectedAt := strVal(item, "collected_at")
		if prev, ok := seenAt[runRef]; ok && collectedAt <= prev {
			continue
		}
		seenAt[runRef] = collectedAt

		p := HistoricalPoint{
			RunRef:             runRef,
			DepartScheduled:    strVal(item, "depart_scheduled"),
			DepartActual:       strVal(item, "depart_actual"),
			ArriveScheduled:    strVal(item, "arrive_scheduled"),
			ArriveActual:       strVal(item, "arrive_actual"),
			TravelMinScheduled: intVal(item, "travel_min_scheduled"),
			DelayMin:           intVal(item, "delay_min"),
		}
		if v, ok := item["travel_min_actual"].(*types.AttributeValueMemberN); ok {
			var i int
			fmt.Sscan(v.Value, &i)
			p.TravelMinActual = &i
		}
		seen[runRef] = p
	}

	trains := make([]HistoricalPoint, 0, len(seen))
	for _, p := range seen {
		trains = append(trains, p)
	}
	sort.Slice(trains, func(i, j int) bool {
		return trains[i].DepartScheduled < trains[j].DepartScheduled
	})

	return &HistoryResult{Route: route, Date: date, Trains: trains}, nil
}

func strVal(item map[string]types.AttributeValue, key string) string {
	if v, ok := item[key].(*types.AttributeValueMemberS); ok {
		return v.Value
	}
	return ""
}

func intVal(item map[string]types.AttributeValue, key string) int {
	if v, ok := item[key].(*types.AttributeValueMemberN); ok {
		var i int
		fmt.Sscan(v.Value, &i)
		return i
	}
	return 0
}
