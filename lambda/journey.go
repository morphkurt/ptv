package main

import (
	"fmt"
	"time"
)

// Stop IDs
const (
	stopTownHall    = 1235
	stopCaulfield   = 1036
	stopMentone     = 1122
	stopSandownPark = 1172
)

// Route IDs
const (
	routeCranbourne = 4
	routePakenham   = 11
	routeFrankston  = 6
)

// Direction IDs
const (
	dirCity       = 1 // inbound (all lines)
	dirCranbourne = 3
	dirPakenham   = 10
	dirFrankston  = 5
)

var melbourneTZ = mustLoadLocation("Australia/Melbourne")

func mustLoadLocation(name string) *time.Location {
	loc, err := time.LoadLocation(name)
	if err != nil {
		panic(err)
	}
	return loc
}

// JourneyResult is the response returned to the caller.
type JourneyResult struct {
	Origin      string           `json:"origin"`
	Destination string           `json:"destination"`
	DepartAt    string           `json:"depart_at"`
	ArriveAt    string           `json:"arrive_at"`
	Transfer    *Transfer        `json:"transfer,omitempty"`
	Disruptions []DisruptionInfo `json:"disruptions,omitempty"`
}

type Transfer struct {
	Station         string `json:"station"`
	ArriveCaulfield string `json:"arrive_caulfield"`
	Line            string `json:"line"`
	DepartCaulfield string `json:"depart_caulfield"`
}

// PlanJourney finds the latest Town Hall departure for the given destination and arrival deadline.
// arriveBy is in Melbourne local time, format "15:04".
// date is optional (format "2006-01-02"); defaults to today in Melbourne time if empty.
// realtime uses estimated departure times from the API where available.
func PlanJourney(client *PTVClient, destination, arriveByStr, date string, realtime bool) (*JourneyResult, error) {
	var base time.Time
	if date == "" {
		base = time.Now().In(melbourneTZ)
	} else {
		var err error
		base, err = time.ParseInLocation("2006-01-02", date, melbourneTZ)
		if err != nil {
			return nil, fmt.Errorf("invalid date format, use YYYY-MM-DD: %w", err)
		}
	}
	arriveBy, err := time.ParseInLocation("15:04", arriveByStr, melbourneTZ)
	if err != nil {
		return nil, fmt.Errorf("invalid arrive_by format, use HH:MM: %w", err)
	}
	arriveBy = time.Date(base.Year(), base.Month(), base.Day(),
		arriveBy.Hour(), arriveBy.Minute(), 0, 0, melbourneTZ)

	// Query window: start 2 hours before the deadline so we see enough candidate trains
	windowStart := arriveBy.Add(-2 * time.Hour)

	switch destination {
	case "sandown_park":
		return planSandownPark(client, arriveBy, windowStart, realtime)
	case "mentone":
		return planMentone(client, arriveBy, windowStart, realtime)
	default:
		return nil, fmt.Errorf("unknown destination %q, use 'mentone' or 'sandown_park'", destination)
	}
}

// planSandownPark finds the latest direct Cranbourne/Pakenham train from Town Hall
// that arrives at Sandown Park by arriveBy.
func planSandownPark(client *PTVClient, arriveBy, windowStart time.Time, realtime bool) (*JourneyResult, error) {
	candidates := []struct{ route, dir int }{
		{routeCranbourne, dirCranbourne},
		{routePakenham, dirPakenham},
	}

	type hit struct {
		departTH time.Time
		arriveSD time.Time
	}
	var best *hit

	for _, c := range candidates {
		deps, err := client.GetDepartures(stopTownHall, c.route, c.dir, 15, windowStart)
		if err != nil {
			return nil, fmt.Errorf("departures from Town Hall: %w", err)
		}
		for _, dep := range deps {
			departTH, err := parseUTC(dep.EffectiveTime(realtime))
			if err != nil {
				continue
			}
			departTH = rebaseToDate(departTH, arriveBy)
			if departTH.After(arriveBy) {
				continue
			}
			arriveSD, err := findStopTimeInPattern(client, dep.RunRef, stopSandownPark, arriveBy, realtime)
			if err != nil || arriveSD.IsZero() {
				continue
			}
			if arriveSD.After(arriveBy) {
				continue
			}
			if best == nil || departTH.After(best.departTH) {
				best = &hit{departTH: departTH, arriveSD: arriveSD}
			}
		}
	}

	if best == nil {
		return nil, fmt.Errorf("no train found from Town Hall to Sandown Park arriving by %s", arriveBy.Format("15:04"))
	}

	return &JourneyResult{
		Origin:      "Town Hall",
		Destination: "Sandown Park",
		DepartAt:    best.departTH.In(melbourneTZ).Format("15:04"),
		ArriveAt:    best.arriveSD.In(melbourneTZ).Format("15:04"),
		Disruptions: activeDisruptions(client, arriveBy, routeCranbourne, routePakenham),
	}, nil
}

// planMentone finds the latest Town Hall departure for the two-leg journey:
// Town Hall → Caulfield (Cranbourne/Pakenham), then Caulfield → Mentone (Frankston).
func planMentone(client *PTVClient, arriveBy, windowStart time.Time, realtime bool) (*JourneyResult, error) {
	// Leg 2: find the latest Frankston departure from Caulfield arriving at Mentone by arriveBy
	frankDeps, err := client.GetDepartures(stopCaulfield, routeFrankston, dirFrankston, 15, windowStart)
	if err != nil {
		return nil, fmt.Errorf("departures from Caulfield (Frankston): %w", err)
	}

	type leg2Hit struct {
		departCaulfield time.Time
		arriveMentone   time.Time
	}
	var bestLeg2 *leg2Hit

	for _, dep := range frankDeps {
		departCaulfield, err := parseUTC(dep.EffectiveTime(realtime))
		if err != nil {
			continue
		}
		departCaulfield = rebaseToDate(departCaulfield, arriveBy)
		if departCaulfield.After(arriveBy) {
			continue
		}
		arriveMentone, err := findStopTimeInPattern(client, dep.RunRef, stopMentone, arriveBy, realtime)
		if err != nil || arriveMentone.IsZero() {
			continue
		}
		if arriveMentone.After(arriveBy) {
			continue
		}
		if bestLeg2 == nil || departCaulfield.After(bestLeg2.departCaulfield) {
			bestLeg2 = &leg2Hit{departCaulfield: departCaulfield, arriveMentone: arriveMentone}
		}
	}

	if bestLeg2 == nil {
		return nil, fmt.Errorf("no Frankston train from Caulfield arriving at Mentone by %s", arriveBy.Format("15:04"))
	}

	// Leg 1: find the latest Cranbourne/Pakenham departure from Town Hall arriving at Caulfield
	// before bestLeg2.departCaulfield
	candidates := []struct{ route, dir int }{
		{routeCranbourne, dirCranbourne},
		{routePakenham, dirPakenham},
	}

	type leg1Hit struct {
		departTH       time.Time
		arriveCaulfield time.Time
	}
	var bestLeg1 *leg1Hit

	for _, c := range candidates {
		deps, err := client.GetDepartures(stopTownHall, c.route, c.dir, 15, windowStart)
		if err != nil {
			return nil, fmt.Errorf("departures from Town Hall: %w", err)
		}
		for _, dep := range deps {
			departTH, err := parseUTC(dep.EffectiveTime(realtime))
			if err != nil {
				continue
			}
			departTH = rebaseToDate(departTH, arriveBy)
			if departTH.After(bestLeg2.departCaulfield) {
				continue
			}
			arriveCaulfield, err := findStopTimeInPattern(client, dep.RunRef, stopCaulfield, arriveBy, realtime)
			if err != nil || arriveCaulfield.IsZero() {
				continue
			}
			// Must arrive at Caulfield at least 5 minutes before the Frankston train departs
			if arriveCaulfield.Add(5 * time.Minute).After(bestLeg2.departCaulfield) {
				continue
			}
			if bestLeg1 == nil || departTH.After(bestLeg1.departTH) {
				bestLeg1 = &leg1Hit{departTH: departTH, arriveCaulfield: arriveCaulfield}
			}
		}
	}

	if bestLeg1 == nil {
		return nil, fmt.Errorf("no train from Town Hall to Caulfield in time for the Frankston connection")
	}

	return &JourneyResult{
		Origin:      "Town Hall",
		Destination: "Mentone",
		DepartAt:    bestLeg1.departTH.In(melbourneTZ).Format("15:04"),
		Transfer: &Transfer{
			Station:         "Caulfield",
			ArriveCaulfield: bestLeg1.arriveCaulfield.In(melbourneTZ).Format("15:04"),
			Line:            "Frankston",
			DepartCaulfield: bestLeg2.departCaulfield.In(melbourneTZ).Format("15:04"),
		},
		ArriveAt:    bestLeg2.arriveMentone.In(melbourneTZ).Format("15:04"),
		Disruptions: activeDisruptions(client, arriveBy, routeCranbourne, routePakenham, routeFrankston),
	}, nil
}

// findStopTimeInPattern fetches the run pattern and returns the scheduled time
// for the given stop_id, rebased to journeyDate in Melbourne time.
// Returns zero time if the stop is not in this run's pattern.
func findStopTimeInPattern(client *PTVClient, runRef string, stopID int, journeyDate time.Time, realtime bool) (time.Time, error) {
	stops, err := client.GetPattern(runRef)
	if err != nil {
		return time.Time{}, err
	}
	for _, s := range stops {
		if s.StopID == stopID {
			t, err := parseUTC(s.EffectiveTime(realtime))
			if err != nil {
				return time.Time{}, err
			}
			return rebaseToDate(t, journeyDate), nil
		}
	}
	return time.Time{}, nil
}

// PlanReturnJourney finds the earliest Town Hall arrival given a departure time from the origin.
// departAt is in Melbourne local time, format "15:04".
// date is optional (format "2006-01-02"); defaults to today in Melbourne time if empty.
func PlanReturnJourney(client *PTVClient, from, departAtStr, date string, realtime bool) (*JourneyResult, error) {
	var base time.Time
	if date == "" {
		base = time.Now().In(melbourneTZ)
	} else {
		var err error
		base, err = time.ParseInLocation("2006-01-02", date, melbourneTZ)
		if err != nil {
			return nil, fmt.Errorf("invalid date format, use YYYY-MM-DD: %w", err)
		}
	}
	departAt, err := time.ParseInLocation("15:04", departAtStr, melbourneTZ)
	if err != nil {
		return nil, fmt.Errorf("invalid depart_at format, use HH:MM: %w", err)
	}
	departAt = time.Date(base.Year(), base.Month(), base.Day(),
		departAt.Hour(), departAt.Minute(), 0, 0, melbourneTZ)

	switch from {
	case "sandown_park":
		return planReturnSandownPark(client, departAt, realtime)
	case "mentone":
		return planReturnMentone(client, departAt, realtime)
	default:
		return nil, fmt.Errorf("unknown origin %q, use 'mentone' or 'sandown_park'", from)
	}
}

// planReturnSandownPark finds the earliest Cranbourne/Pakenham train from Sandown Park
// departing at or after departAt, and returns its arrival time at Town Hall.
func planReturnSandownPark(client *PTVClient, departAt time.Time, realtime bool) (*JourneyResult, error) {
	candidates := []struct{ route, dir int }{
		{routeCranbourne, dirCity},
		{routePakenham, dirCity},
	}

	type hit struct {
		departSD  time.Time
		arriveTH  time.Time
	}
	var best *hit

	for _, c := range candidates {
		deps, err := client.GetDepartures(stopSandownPark, c.route, c.dir, 10, departAt)
		if err != nil {
			return nil, fmt.Errorf("departures from Sandown Park: %w", err)
		}
		for _, dep := range deps {
			departSD, err := parseUTC(dep.EffectiveTime(realtime))
			if err != nil {
				continue
			}
			departSD = rebaseToDate(departSD, departAt)
			if departSD.Before(departAt) {
				continue
			}
			arriveTH, err := findStopTimeInPattern(client, dep.RunRef, stopTownHall, departAt, realtime)
			if err != nil || arriveTH.IsZero() {
				continue
			}
			if best == nil || departSD.Before(best.departSD) {
				best = &hit{departSD: departSD, arriveTH: arriveTH}
			}
		}
	}

	if best == nil {
		return nil, fmt.Errorf("no train found from Sandown Park to Town Hall departing after %s", departAt.Format("15:04"))
	}

	return &JourneyResult{
		Origin:      "Sandown Park",
		Destination: "Town Hall",
		DepartAt:    best.departSD.In(melbourneTZ).Format("15:04"),
		ArriveAt:    best.arriveTH.In(melbourneTZ).Format("15:04"),
		Disruptions: activeDisruptions(client, departAt, routeCranbourne, routePakenham),
	}, nil
}

// planReturnMentone finds the earliest return journey from Mentone to Town Hall
// departing at or after departAt, via Caulfield (Frankston → Cranbourne/Pakenham).
func planReturnMentone(client *PTVClient, departAt time.Time, realtime bool) (*JourneyResult, error) {
	// Leg 1: Mentone → Caulfield on Frankston (City direction)
	frankDeps, err := client.GetDepartures(stopMentone, routeFrankston, dirCity, 10, departAt)
	if err != nil {
		return nil, fmt.Errorf("departures from Mentone (Frankston): %w", err)
	}

	type leg1Hit struct {
		departMentone   time.Time
		arriveCaulfield time.Time
	}
	var bestLeg1 *leg1Hit

	for _, dep := range frankDeps {
		departMentone, err := parseUTC(dep.EffectiveTime(realtime))
		if err != nil {
			continue
		}
		departMentone = rebaseToDate(departMentone, departAt)
		if departMentone.Before(departAt) {
			continue
		}
		arriveCaulfield, err := findStopTimeInPattern(client, dep.RunRef, stopCaulfield, departAt, realtime)
		if err != nil || arriveCaulfield.IsZero() {
			continue
		}
		if bestLeg1 == nil || departMentone.Before(bestLeg1.departMentone) {
			bestLeg1 = &leg1Hit{departMentone: departMentone, arriveCaulfield: arriveCaulfield}
		}
	}

	if bestLeg1 == nil {
		return nil, fmt.Errorf("no Frankston train from Mentone to Caulfield departing after %s", departAt.Format("15:04"))
	}

	// Leg 2: Caulfield → Town Hall on Cranbourne/Pakenham (City direction), at least 5 min after arriving
	connectAfter := bestLeg1.arriveCaulfield.Add(5 * time.Minute)
	candidates := []struct{ route, dir int }{
		{routeCranbourne, dirCity},
		{routePakenham, dirCity},
	}

	type leg2Hit struct {
		departCaulfield time.Time
		arriveTH        time.Time
	}
	var bestLeg2 *leg2Hit

	for _, c := range candidates {
		deps, err := client.GetDepartures(stopCaulfield, c.route, c.dir, 10, connectAfter)
		if err != nil {
			return nil, fmt.Errorf("departures from Caulfield: %w", err)
		}
		for _, dep := range deps {
			departCaulfield, err := parseUTC(dep.EffectiveTime(realtime))
			if err != nil {
				continue
			}
			departCaulfield = rebaseToDate(departCaulfield, departAt)
			if departCaulfield.Before(connectAfter) {
				continue
			}
			arriveTH, err := findStopTimeInPattern(client, dep.RunRef, stopTownHall, departAt, realtime)
			if err != nil || arriveTH.IsZero() {
				continue
			}
			if bestLeg2 == nil || departCaulfield.Before(bestLeg2.departCaulfield) {
				bestLeg2 = &leg2Hit{departCaulfield: departCaulfield, arriveTH: arriveTH}
			}
		}
	}

	if bestLeg2 == nil {
		return nil, fmt.Errorf("no Cranbourne/Pakenham train from Caulfield to Town Hall after Frankston connection")
	}

	return &JourneyResult{
		Origin:      "Mentone",
		Destination: "Town Hall",
		DepartAt:    bestLeg1.departMentone.In(melbourneTZ).Format("15:04"),
		Transfer: &Transfer{
			Station:         "Caulfield",
			ArriveCaulfield: bestLeg1.arriveCaulfield.In(melbourneTZ).Format("15:04"),
			Line:            "Cranbourne / Pakenham",
			DepartCaulfield: bestLeg2.departCaulfield.In(melbourneTZ).Format("15:04"),
		},
		ArriveAt:    bestLeg2.arriveTH.In(melbourneTZ).Format("15:04"),
		Disruptions: activeDisruptions(client, departAt, routeCranbourne, routePakenham, routeFrankston),
	}, nil
}

// PlanOutboundJourney finds the earliest Town Hall departure for the given destination,
// given a departure time from Town Hall (depart-at mode).
func PlanOutboundJourney(client *PTVClient, destination, departAtStr, date string, realtime bool) (*JourneyResult, error) {
	var base time.Time
	if date == "" {
		base = time.Now().In(melbourneTZ)
	} else {
		var err error
		base, err = time.ParseInLocation("2006-01-02", date, melbourneTZ)
		if err != nil {
			return nil, fmt.Errorf("invalid date format, use YYYY-MM-DD: %w", err)
		}
	}
	departAt, err := time.ParseInLocation("15:04", departAtStr, melbourneTZ)
	if err != nil {
		return nil, fmt.Errorf("invalid depart_at format, use HH:MM: %w", err)
	}
	departAt = time.Date(base.Year(), base.Month(), base.Day(),
		departAt.Hour(), departAt.Minute(), 0, 0, melbourneTZ)

	switch destination {
	case "sandown_park":
		return planOutboundSandownPark(client, departAt, realtime)
	case "mentone":
		return planOutboundMentone(client, departAt, realtime)
	default:
		return nil, fmt.Errorf("unknown destination %q, use 'mentone' or 'sandown_park'", destination)
	}
}

// planOutboundSandownPark finds the earliest direct Cranbourne/Pakenham train from Town Hall
// departing at or after departAt, and returns its arrival time at Sandown Park.
func planOutboundSandownPark(client *PTVClient, departAt time.Time, realtime bool) (*JourneyResult, error) {
	candidates := []struct{ route, dir int }{
		{routeCranbourne, dirCranbourne},
		{routePakenham, dirPakenham},
	}

	type hit struct {
		departTH time.Time
		arriveSD time.Time
	}
	var best *hit

	for _, c := range candidates {
		deps, err := client.GetDepartures(stopTownHall, c.route, c.dir, 10, departAt)
		if err != nil {
			return nil, fmt.Errorf("departures from Town Hall: %w", err)
		}
		for _, dep := range deps {
			departTH, err := parseUTC(dep.EffectiveTime(realtime))
			if err != nil {
				continue
			}
			departTH = rebaseToDate(departTH, departAt)
			if departTH.Before(departAt) {
				continue
			}
			arriveSD, err := findStopTimeInPattern(client, dep.RunRef, stopSandownPark, departAt, realtime)
			if err != nil || arriveSD.IsZero() {
				continue
			}
			if best == nil || departTH.Before(best.departTH) {
				best = &hit{departTH: departTH, arriveSD: arriveSD}
			}
		}
	}

	if best == nil {
		return nil, fmt.Errorf("no train from Town Hall to Sandown Park departing after %s", departAt.Format("15:04"))
	}

	return &JourneyResult{
		Origin:      "Town Hall",
		Destination: "Sandown Park",
		DepartAt:    best.departTH.In(melbourneTZ).Format("15:04"),
		ArriveAt:    best.arriveSD.In(melbourneTZ).Format("15:04"),
		Disruptions: activeDisruptions(client, departAt, routeCranbourne, routePakenham),
	}, nil
}

// planOutboundMentone finds the earliest Town Hall → Caulfield (Cranbourne/Pakenham) →
// Mentone (Frankston) journey departing at or after departAt.
func planOutboundMentone(client *PTVClient, departAt time.Time, realtime bool) (*JourneyResult, error) {
	// Leg 1: Town Hall → Caulfield on Cranbourne/Pakenham
	candidates := []struct{ route, dir int }{
		{routeCranbourne, dirCranbourne},
		{routePakenham, dirPakenham},
	}

	type leg1Hit struct {
		departTH        time.Time
		arriveCaulfield time.Time
	}
	var bestLeg1 *leg1Hit

	for _, c := range candidates {
		deps, err := client.GetDepartures(stopTownHall, c.route, c.dir, 10, departAt)
		if err != nil {
			return nil, fmt.Errorf("departures from Town Hall: %w", err)
		}
		for _, dep := range deps {
			departTH, err := parseUTC(dep.EffectiveTime(realtime))
			if err != nil {
				continue
			}
			departTH = rebaseToDate(departTH, departAt)
			if departTH.Before(departAt) {
				continue
			}
			arriveCaulfield, err := findStopTimeInPattern(client, dep.RunRef, stopCaulfield, departAt, realtime)
			if err != nil || arriveCaulfield.IsZero() {
				continue
			}
			if bestLeg1 == nil || departTH.Before(bestLeg1.departTH) {
				bestLeg1 = &leg1Hit{departTH: departTH, arriveCaulfield: arriveCaulfield}
			}
		}
	}

	if bestLeg1 == nil {
		return nil, fmt.Errorf("no train from Town Hall departing after %s", departAt.Format("15:04"))
	}

	// Leg 2: Caulfield → Mentone on Frankston, at least 5 min after arriving
	connectAfter := bestLeg1.arriveCaulfield.Add(5 * time.Minute)
	frankDeps, err := client.GetDepartures(stopCaulfield, routeFrankston, dirFrankston, 10, connectAfter)
	if err != nil {
		return nil, fmt.Errorf("departures from Caulfield (Frankston): %w", err)
	}

	type leg2Hit struct {
		departCaulfield time.Time
		arriveMentone   time.Time
	}
	var bestLeg2 *leg2Hit

	for _, dep := range frankDeps {
		departCaulfield, err := parseUTC(dep.EffectiveTime(realtime))
		if err != nil {
			continue
		}
		departCaulfield = rebaseToDate(departCaulfield, departAt)
		if departCaulfield.Before(connectAfter) {
			continue
		}
		arriveMentone, err := findStopTimeInPattern(client, dep.RunRef, stopMentone, departAt, realtime)
		if err != nil || arriveMentone.IsZero() {
			continue
		}
		if bestLeg2 == nil || departCaulfield.Before(bestLeg2.departCaulfield) {
			bestLeg2 = &leg2Hit{departCaulfield: departCaulfield, arriveMentone: arriveMentone}
		}
	}

	if bestLeg2 == nil {
		return nil, fmt.Errorf("no Frankston train from Caulfield to Mentone after connection")
	}

	return &JourneyResult{
		Origin:      "Town Hall",
		Destination: "Mentone",
		DepartAt:    bestLeg1.departTH.In(melbourneTZ).Format("15:04"),
		Transfer: &Transfer{
			Station:         "Caulfield",
			ArriveCaulfield: bestLeg1.arriveCaulfield.In(melbourneTZ).Format("15:04"),
			Line:            "Frankston",
			DepartCaulfield: bestLeg2.departCaulfield.In(melbourneTZ).Format("15:04"),
		},
		ArriveAt:    bestLeg2.arriveMentone.In(melbourneTZ).Format("15:04"),
		Disruptions: activeDisruptions(client, departAt, routeCranbourne, routePakenham, routeFrankston),
	}, nil
}

// activeDisruptions fetches disruptions for the given routes and returns those
// that are active on journeyDate, deduplicated by disruption ID.
func activeDisruptions(client *PTVClient, journeyDate time.Time, routeIDs ...int) []DisruptionInfo {
	seen := map[int]bool{}
	var result []DisruptionInfo

	for _, routeID := range routeIDs {
		disruptions, err := client.GetDisruptions(routeID)
		if err != nil {
			continue
		}
		for _, d := range disruptions {
			if seen[d.ID] {
				continue
			}
			// Parse from_date; skip if it hasn't started yet
			from, err := time.Parse(time.RFC3339, d.FromDate)
			if err != nil || journeyDate.Before(from) {
				continue
			}
			// If to_date is set, skip if the disruption has already ended
			if d.ToDate != "" {
				to, err := time.Parse(time.RFC3339, d.ToDate)
				if err == nil && journeyDate.After(to) {
					continue
				}
			}
			seen[d.ID] = true
			result = append(result, d)
		}
	}
	return result
}

// rebaseToDate takes the Melbourne time-of-day from apiTime and returns that
// time on journeyDate. This normalises times from the API (which may return
// today's schedule) so they compare correctly against a future arriveBy.
func rebaseToDate(apiTime, journeyDate time.Time) time.Time {
	local := apiTime.In(melbourneTZ)
	base := journeyDate.In(melbourneTZ)
	return time.Date(base.Year(), base.Month(), base.Day(),
		local.Hour(), local.Minute(), local.Second(), 0, melbourneTZ)
}

func parseUTC(s string) (time.Time, error) {
	return time.Parse(time.RFC3339, s)
}
