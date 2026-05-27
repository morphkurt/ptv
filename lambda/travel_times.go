package main

import (
	"fmt"
	"math"
	"sort"
	"sync"
	"time"
)

// StopTimes holds both scheduled and estimated times for a stop in a run's pattern.
type StopTimes struct {
	Scheduled   time.Time
	Estimated   time.Time
	HasEstimate bool
}

type TrainPoint struct {
	RunRef             string `json:"run_ref,omitempty"`
	DepartScheduled    string `json:"depart_scheduled"`
	DepartEstimated    string `json:"depart_estimated,omitempty"`
	ArriveScheduled    string `json:"arrive_scheduled"`
	ArriveEstimated    string `json:"arrive_estimated,omitempty"`
	TravelMinScheduled int    `json:"travel_min_scheduled"`
	TravelMinEstimated *int   `json:"travel_min_estimated,omitempty"`
	DelayMin           int    `json:"delay_min"`
}

type WindowData struct {
	From   string       `json:"from"`
	To     string       `json:"to"`
	Trains []TrainPoint `json:"trains"`
}

type TravelTimesResult struct {
	Route   string     `json:"route"`
	Date    string     `json:"date"`
	Morning WindowData `json:"morning"`
	Evening WindowData `json:"evening"`
}

type routeDir struct{ route, dir int }

// GetTravelTimes returns all trains in the morning (07:00-09:30) and evening
// (16:00-18:00) windows for a given route, with both scheduled and estimated times.
func GetTravelTimes(client *PTVClient, route, date string) (*TravelTimesResult, error) {
	var journeyDate time.Time
	if date == "" {
		journeyDate = time.Now().In(melbourneTZ)
	} else {
		var err error
		journeyDate, err = time.ParseInLocation("2006-01-02", date, melbourneTZ)
		if err != nil {
			return nil, fmt.Errorf("invalid date: %w", err)
		}
	}

	at := func(hhmm string) time.Time {
		t, _ := time.ParseInLocation("15:04", hhmm, melbourneTZ)
		return time.Date(journeyDate.Year(), journeyDate.Month(), journeyDate.Day(),
			t.Hour(), t.Minute(), 0, 0, melbourneTZ)
	}

	morningFrom, morningTo := at("07:00"), at("09:30")
	eveningFrom, eveningTo := at("16:00"), at("18:00")

	result := &TravelTimesResult{
		Route:   route,
		Date:    journeyDate.Format("2006-01-02"),
		Morning: WindowData{From: "07:00", To: "09:30"},
		Evening: WindowData{From: "16:00", To: "18:00"},
	}

	cityRoutes    := []routeDir{{routeCranbourne, dirCity}, {routePakenham, dirCity}}
	outboundRoutes := []routeDir{{routeCranbourne, dirCranbourne}, {routePakenham, dirPakenham}}
	frankstonIn   := []routeDir{{routeFrankston, dirCity}}
	frankstonOut  := []routeDir{{routeFrankston, dirFrankston}}

	switch route {
	case "sandown_park_inbound":
		result.Morning.Trains = collectDirect(client, stopSandownPark, stopTownHall, cityRoutes, morningFrom, morningTo, journeyDate)
		result.Evening.Trains = collectDirect(client, stopSandownPark, stopTownHall, cityRoutes, eveningFrom, eveningTo, journeyDate)

	case "sandown_park_outbound":
		result.Morning.Trains = collectDirect(client, stopTownHall, stopSandownPark, outboundRoutes, morningFrom, morningTo, journeyDate)
		result.Evening.Trains = collectDirect(client, stopTownHall, stopSandownPark, outboundRoutes, eveningFrom, eveningTo, journeyDate)

	case "mentone_inbound":
		// Mentone → Caulfield (Frankston City) → Town Hall (Cranbourne/Pakenham City)
		result.Morning.Trains = collectTransfer(client, stopMentone, frankstonIn, stopCaulfield, cityRoutes, stopTownHall, morningFrom, morningTo, journeyDate)
		result.Evening.Trains = collectTransfer(client, stopMentone, frankstonIn, stopCaulfield, cityRoutes, stopTownHall, eveningFrom, eveningTo, journeyDate)

	case "mentone_outbound":
		// Town Hall → Caulfield (Cranbourne/Pakenham outbound) → Mentone (Frankston outbound)
		result.Morning.Trains = collectTransfer(client, stopTownHall, outboundRoutes, stopCaulfield, frankstonOut, stopMentone, morningFrom, morningTo, journeyDate)
		result.Evening.Trains = collectTransfer(client, stopTownHall, outboundRoutes, stopCaulfield, frankstonOut, stopMentone, eveningFrom, eveningTo, journeyDate)

	default:
		return nil, fmt.Errorf("unknown route %q — use: mentone_inbound, sandown_park_inbound, mentone_outbound, sandown_park_outbound", route)
	}

	return result, nil
}

// collectDirect gathers travel time data for a direct (single-leg) route.
func collectDirect(
	client *PTVClient,
	originStop, destStop int,
	routes []routeDir,
	windowFrom, windowTo, journeyDate time.Time,
) []TrainPoint {
	deps := fetchDepsInWindow(client, originStop, routes, windowFrom, windowTo, journeyDate)

	arrivals := parallelStopTimes(client, deps, destStop, journeyDate)

	var points []TrainPoint
	for i, d := range deps {
		arr := arrivals[i]
		if arr == nil || arr.Scheduled.IsZero() {
			continue
		}
		schedMin := roundMinutes(arr.Scheduled.Sub(d.departSch))
		if schedMin <= 0 {
			continue
		}
		pt := TrainPoint{
			RunRef:             d.runRef,
			DepartScheduled:    d.departSch.In(melbourneTZ).Format("15:04"),
			ArriveScheduled:    arr.Scheduled.In(melbourneTZ).Format("15:04"),
			TravelMinScheduled: schedMin,
		}
		if d.hasEst && arr.HasEstimate {
			estMin := roundMinutes(arr.Estimated.Sub(d.departEst))
			delay := roundMinutes(d.departEst.Sub(d.departSch))
			pt.DepartEstimated = d.departEst.In(melbourneTZ).Format("15:04")
			pt.ArriveEstimated = arr.Estimated.In(melbourneTZ).Format("15:04")
			pt.TravelMinEstimated = intPtr(estMin)
			pt.DelayMin = delay
		}
		points = append(points, pt)
	}
	return points
}

// collectTransfer gathers travel time data for a two-leg journey via a transfer stop.
func collectTransfer(
	client *PTVClient,
	originStop int, leg1Routes []routeDir,
	transferStop int, leg2Routes []routeDir,
	destStop int,
	windowFrom, windowTo, journeyDate time.Time,
) []TrainPoint {
	// Leg 1: origin → transfer stop
	leg1Deps := fetchDepsInWindow(client, originStop, leg1Routes, windowFrom, windowTo, journeyDate)
	transferArrivals := parallelStopTimes(client, leg1Deps, transferStop, journeyDate)

	// Leg 2: fetch all Cranbourne/Pakenham/Frankston departures from transfer stop in extended window
	// (extended by 1h to catch connections after the last origin train arrives)
	extendedTo := windowTo.Add(1 * time.Hour)
	leg2Deps := fetchDepsInWindow(client, transferStop, leg2Routes, windowFrom, extendedTo, journeyDate)
	destArrivals := parallelStopTimes(client, leg2Deps, destStop, journeyDate)

	var points []TrainPoint
	for i, leg1 := range leg1Deps {
		transArr := transferArrivals[i]
		if transArr == nil || transArr.Scheduled.IsZero() {
			continue
		}

		// Find the first leg 2 train departing ≥ transfer arrival + 5 min (scheduled)
		connectAfter := transArr.Scheduled.Add(5 * time.Minute)
		connIdx := -1
		for k, leg2 := range leg2Deps {
			if !leg2.departSch.Before(connectAfter) &&
				destArrivals[k] != nil && !destArrivals[k].Scheduled.IsZero() {
				connIdx = k
				break
			}
		}
		if connIdx < 0 {
			continue
		}

		conn := leg2Deps[connIdx]
		destArr := destArrivals[connIdx]

		schedMin := roundMinutes(destArr.Scheduled.Sub(leg1.departSch))
		if schedMin <= 0 {
			continue
		}

		pt := TrainPoint{
			RunRef:             leg1.runRef,
			DepartScheduled:    leg1.departSch.In(melbourneTZ).Format("15:04"),
			ArriveScheduled:    destArr.Scheduled.In(melbourneTZ).Format("15:04"),
			TravelMinScheduled: schedMin,
		}

		if leg1.hasEst && transArr.HasEstimate && conn.hasEst && destArr.HasEstimate {
			estConnectAfter := transArr.Estimated.Add(5 * time.Minute)
			if !conn.departEst.Before(estConnectAfter) {
				estMin := roundMinutes(destArr.Estimated.Sub(leg1.departEst))
				delay := roundMinutes(leg1.departEst.Sub(leg1.departSch))
				pt.DepartEstimated = leg1.departEst.In(melbourneTZ).Format("15:04")
				pt.ArriveEstimated = destArr.Estimated.In(melbourneTZ).Format("15:04")
				pt.TravelMinEstimated = intPtr(estMin)
				pt.DelayMin = delay
			}
		}
		points = append(points, pt)
	}
	return points
}

// ── Helpers ───────────────────────────────────────────────────────────────────

type depJob struct {
	runRef    string
	departSch time.Time
	departEst time.Time
	hasEst    bool
}

// fetchDepsInWindow queries departures across multiple routes, deduplicates,
// filters to the given window, and sorts by scheduled departure time.
func fetchDepsInWindow(
	client *PTVClient,
	stopID int,
	routes []routeDir,
	windowFrom, windowTo, journeyDate time.Time,
) []depJob {
	seen := map[string]bool{}
	var all []Departure
	for _, r := range routes {
		deps, err := client.GetDepartures(stopID, r.route, r.dir, 40, windowFrom)
		if err != nil {
			continue
		}
		for _, d := range deps {
			if !seen[d.RunRef] {
				seen[d.RunRef] = true
				all = append(all, d)
			}
		}
	}

	var jobs []depJob
	for _, dep := range all {
		t, err := parseUTC(dep.ScheduledDeparture)
		if err != nil {
			continue
		}
		t = rebaseToDate(t, journeyDate)
		if t.Before(windowFrom) || t.After(windowTo) {
			continue
		}
		j := depJob{runRef: dep.RunRef, departSch: t}
		if dep.EstimatedDeparture != "" {
			est, err := parseUTC(dep.EstimatedDeparture)
			if err == nil {
				j.departEst = rebaseToDate(est, journeyDate)
				j.hasEst = true
			}
		}
		jobs = append(jobs, j)
	}
	sort.Slice(jobs, func(i, k int) bool {
		return jobs[i].departSch.Before(jobs[k].departSch)
	})
	return jobs
}

// parallelStopTimes fetches the stop times for a given stop ID from each job's
// run pattern, running all lookups concurrently.
func parallelStopTimes(client *PTVClient, jobs []depJob, stopID int, journeyDate time.Time) []*StopTimes {
	results := make([]*StopTimes, len(jobs))
	var wg sync.WaitGroup
	for i, j := range jobs {
		wg.Add(1)
		go func(i int, runRef string) {
			defer wg.Done()
			results[i], _ = findBothStopTimesInPattern(client, runRef, stopID, journeyDate)
		}(i, j.runRef)
	}
	wg.Wait()
	return results
}

// findBothStopTimesInPattern returns both scheduled and estimated times for a
// stop in a run's pattern, rebased to journeyDate.
func findBothStopTimesInPattern(client *PTVClient, runRef string, stopID int, journeyDate time.Time) (*StopTimes, error) {
	stops, err := client.GetPattern(runRef)
	if err != nil {
		return nil, err
	}
	for _, s := range stops {
		if s.StopID != stopID {
			continue
		}
		scheduled, err := parseUTC(s.ScheduledDeparture)
		if err != nil {
			return nil, err
		}
		result := &StopTimes{Scheduled: rebaseToDate(scheduled, journeyDate)}
		if s.EstimatedDeparture != "" {
			est, err := parseUTC(s.EstimatedDeparture)
			if err == nil {
				result.Estimated = rebaseToDate(est, journeyDate)
				result.HasEstimate = true
			}
		}
		return result, nil
	}
	return nil, nil
}

func roundMinutes(d time.Duration) int { return int(math.Round(d.Minutes())) }
func intPtr(v int) *int                { return &v }
