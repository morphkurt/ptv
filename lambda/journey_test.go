package main

import (
	"fmt"
	"os"
	"testing"
)

func client() *PTVClient {
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

func TestSandownPark(t *testing.T) {
	c := client()
	results, err := PlanJourney(c, "sandown_park", "17:45", "2026-05-28", false)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	for i, r := range results {
		fmt.Printf("Sandown Park option %d: depart Town Hall %s, arrive %s\n", i+1, r.DepartAt, r.ArriveAt)
	}
}

func TestMentone(t *testing.T) {
	c := client()
	results, err := PlanJourney(c, "mentone", "17:30", "2026-05-28", false)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	for i, r := range results {
		fmt.Printf("Mentone option %d: depart Town Hall %s, transfer at %s (arrive %s, depart %s on %s), arrive Mentone %s, express=%v\n",
			i+1,
			r.DepartAt,
			r.Transfer.Station,
			r.Transfer.ArriveCaulfield,
			r.Transfer.DepartCaulfield,
			r.Transfer.Line,
			r.ArriveAt,
			r.Express,
		)
	}
}

func TestPattern967412(t *testing.T) {
	c := client()
	stops, err := c.GetPattern("967412")
	if err != nil {
		t.Fatalf("GetPattern: %v", err)
	}
	fmt.Printf("Pattern for 967412 (%d stops):\n", len(stops))
	for _, s := range stops {
		fmt.Printf("  stop_id=%d sched=%q est=%q\n", s.StopID, s.ScheduledDeparture, s.EstimatedDeparture)
	}
}
