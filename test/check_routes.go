package main

import (
	"crypto/hmac"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

const (
	baseURL = "https://timetableapi.ptv.vic.gov.au"
	devID   = "1000080"
	apiKey  = "786416fc-aa6c-11e3-8bed-0263a9d0b8a0"
)

func sign(path string) string {
	if strings.Contains(path, "?") {
		path += "&devid=" + devID
	} else {
		path += "?devid=" + devID
	}
	mac := hmac.New(sha1.New, []byte(apiKey))
	mac.Write([]byte(path))
	sig := hex.EncodeToString(mac.Sum(nil))
	return baseURL + path + "&signature=" + strings.ToUpper(sig)
}

func get(path string) []byte {
	resp, _ := http.Get(sign(path))
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return body
}

type Departure struct {
	RouteID       int    `json:"route_id"`
	DirectionID   int    `json:"direction_id"`
	ScheduledDeparture string `json:"scheduled_departure_utc"`
	EstimatedDeparture string `json:"estimated_departure_utc"`
	RunRef        string `json:"run_ref"`
}

type StopInfo struct {
	StopID   int    `json:"stop_id"`
	StopName string `json:"stop_name"`
}

func departures(stopID int, label string) {
	path := fmt.Sprintf("/v3/departures/route_type/0/stop/%d?max_results=3&expand=Route&expand=Direction", stopID)
	body := get(path)
	var result struct {
		Departures []struct {
			RouteID     int    `json:"route_id"`
			DirectionID int    `json:"direction_id"`
			ScheduledDeparture string `json:"scheduled_departure_utc"`
			RunRef      string `json:"run_ref"`
		} `json:"departures"`
		Routes map[string]struct {
			RouteName string `json:"route_name"`
		} `json:"routes"`
		Directions map[string]struct {
			DirectionName string `json:"direction_name"`
		} `json:"directions"`
	}
	json.Unmarshal(body, &result)
	fmt.Printf("=== %s (stop %d) ===\n", label, stopID)
	for _, d := range result.Departures {
		routeKey := fmt.Sprintf("%d", d.RouteID)
		dirKey := fmt.Sprintf("%d", d.DirectionID)
		fmt.Printf("  route=%-12s dir=%-20s sched=%s run=%s\n",
			result.Routes[routeKey].RouteName,
			result.Directions[dirKey].DirectionName,
			d.ScheduledDeparture,
			d.RunRef,
		)
	}
}

func main() {
	departures(1235, "Town Hall")
	departures(1036, "Caulfield")
	departures(1122, "Mentone")
	departures(1172, "Sandown Park")
}
