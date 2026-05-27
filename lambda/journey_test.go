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
	result, err := PlanJourney(c, "sandown_park", "17:45", "2026-05-28", false)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	fmt.Printf("Sandown Park result: depart Town Hall %s, arrive %s\n",
		result.DepartAt, result.ArriveAt)
}

func TestMentone(t *testing.T) {
	c := client()
	result, err := PlanJourney(c, "mentone", "17:30", "2026-05-28", false)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	fmt.Printf("Mentone result: depart Town Hall %s, transfer at %s (arrive %s, depart %s on %s), arrive Mentone %s\n",
		result.DepartAt,
		result.Transfer.Station,
		result.Transfer.ArriveCaulfield,
		result.Transfer.DepartCaulfield,
		result.Transfer.Line,
		result.ArriveAt,
	)
}
