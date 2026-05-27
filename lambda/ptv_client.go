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
	"time"
)

const ptvBase = "https://timetableapi.ptv.vic.gov.au"

type PTVClient struct {
	devID  string
	apiKey string
}

func NewPTVClient(devID, apiKey string) *PTVClient {
	return &PTVClient{devID: devID, apiKey: apiKey}
}

func (c *PTVClient) sign(path string) string {
	if strings.Contains(path, "?") {
		path += "&devid=" + c.devID
	} else {
		path += "?devid=" + c.devID
	}
	mac := hmac.New(sha1.New, []byte(c.apiKey))
	mac.Write([]byte(path))
	sig := hex.EncodeToString(mac.Sum(nil))
	return ptvBase + path + "&signature=" + strings.ToUpper(sig)
}

func (c *PTVClient) get(path string, out any) error {
	resp, err := http.Get(c.sign(path))
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("PTV API %d: %s", resp.StatusCode, string(body))
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

// Departure represents a single departure from the PTV API.
type Departure struct {
	StopID             int    `json:"stop_id"`
	RouteID            int    `json:"route_id"`
	RunRef             string `json:"run_ref"`
	DirectionID        int    `json:"direction_id"`
	ScheduledDeparture string `json:"scheduled_departure_utc"`
	EstimatedDeparture string `json:"estimated_departure_utc"`
}

// EffectiveTime returns the estimated departure time when realtime is true and
// an estimate is available, otherwise falls back to the scheduled time.
func (d Departure) EffectiveTime(realtime bool) string {
	if realtime && d.EstimatedDeparture != "" {
		return d.EstimatedDeparture
	}
	return d.ScheduledDeparture
}

type DeparturesResponse struct {
	Departures []Departure `json:"departures"`
}

type PatternResponse struct {
	Departures []Departure `json:"departures"`
}

type DisruptionInfo struct {
	ID          int    `json:"id"`
	Title       string `json:"title"`
	Description string `json:"description"`
	Status      string `json:"status"`
	FromDate    string `json:"from_date"`
	ToDate      string `json:"to_date,omitempty"`
}

// GetDisruptions returns metro train disruptions for a given route.
func (c *PTVClient) GetDisruptions(routeID int) ([]DisruptionInfo, error) {
	path := fmt.Sprintf("/v3/disruptions/route/%d?disruption_status=current", routeID)
	var resp struct {
		Disruptions struct {
			MetroTrain []struct {
				DisruptionID int    `json:"disruption_id"`
				Title        string `json:"title"`
				Description  string `json:"description"`
				Status       string `json:"disruption_status"`
				FromDate     string `json:"from_date"`
				ToDate       string `json:"to_date"`
			} `json:"metro_train"`
		} `json:"disruptions"`
	}
	if err := c.get(path, &resp); err != nil {
		return nil, err
	}
	out := make([]DisruptionInfo, 0, len(resp.Disruptions.MetroTrain))
	for _, d := range resp.Disruptions.MetroTrain {
		out = append(out, DisruptionInfo{
			ID:          d.DisruptionID,
			Title:       d.Title,
			Description: d.Description,
			Status:      d.Status,
			FromDate:    d.FromDate,
			ToDate:      d.ToDate,
		})
	}
	return out, nil
}

// GetDepartures fetches departures from a stop for a specific route and direction.
// fromUTC anchors the query window (departures on or after this time); zero means now.
func (c *PTVClient) GetDepartures(stopID, routeID, directionID, maxResults int, fromUTC time.Time) ([]Departure, error) {
	path := fmt.Sprintf("/v3/departures/route_type/0/stop/%d/route/%d?direction_id=%d&max_results=%d",
		stopID, routeID, directionID, maxResults)
	if !fromUTC.IsZero() {
		path += "&date_utc=" + fromUTC.UTC().Format(time.RFC3339)
	}
	var resp DeparturesResponse
	if err := c.get(path, &resp); err != nil {
		return nil, err
	}
	return resp.Departures, nil
}

// GetPattern fetches the full stopping pattern for a run.
func (c *PTVClient) GetPattern(runRef string) ([]Departure, error) {
	path := fmt.Sprintf("/v3/pattern/run/%s/route_type/0", runRef)
	var resp PatternResponse
	if err := c.get(path, &resp); err != nil {
		return nil, err
	}
	return resp.Departures, nil
}
