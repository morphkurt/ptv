package main

import (
	"fmt"
	"sort"
	"sync"
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

// Intermediate stop counts for a stopping-all-stations Frankston service.
// Express services have fewer stops than these thresholds.
const (
	frankInboundFullStops  = 9 // Mentone→Caulfield (city direction)
	frankOutboundFullStops = 5 // Caulfield→Mentone (Frankston direction)
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
	Express     bool             `json:"express,omitempty"`
	Transfer    *Transfer        `json:"transfer,omitempty"`
	Disruptions []DisruptionInfo `json:"disruptions,omitempty"`
	TrackRunRef string           `json:"track_run_ref,omitempty"` // run that reaches the final destination
	Leg1RunRef  string           `json:"leg1_run_ref,omitempty"`  // first-leg run (transfer routes only)
}

type Transfer struct {
	Station         string `json:"station"`
	ArriveCaulfield string `json:"arrive_caulfield"`
	Line            string `json:"line"`
	DepartCaulfield string `json:"depart_caulfield"`
}

// PlanJourney finds up to 3 latest Town Hall departures for the given destination arriving by arriveBy.
// arriveBy is in Melbourne local time, format "15:04".
// date is optional (format "2006-01-02"); defaults to today in Melbourne time if empty.
func PlanJourney(client *PTVClient, destination, arriveByStr, date string, realtime bool) ([]JourneyResult, error) {
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

// planSandownPark finds up to 3 latest direct C/P trains from Town Hall arriving at Sandown Park by arriveBy.
func planSandownPark(client *PTVClient, arriveBy, windowStart time.Time, realtime bool) ([]JourneyResult, error) {
	var allDeps []Departure
	seen := map[string]bool{}
	for _, c := range []struct{ route, dir int }{{routeCranbourne, dirCranbourne}, {routePakenham, dirPakenham}} {
		deps, err := client.GetDepartures(stopTownHall, c.route, c.dir, 20, windowStart)
		if err != nil {
			return nil, fmt.Errorf("departures from Town Hall: %w", err)
		}
		for _, d := range deps {
			if !seen[d.RunRef] {
				seen[d.RunRef] = true
				allDeps = append(allDeps, d)
			}
		}
	}

	type candidate struct {
		dep      Departure
		departTH time.Time
	}
	var candidates []candidate
	for _, dep := range allDeps {
		t, err := parseUTC(dep.EffectiveTime(realtime))
		if err != nil {
			continue
		}
		t = rebaseToDate(t, arriveBy)
		if t.After(arriveBy) {
			continue
		}
		candidates = append(candidates, candidate{dep, t})
	}

	// Parallel-fetch Sandown Park arrival for all candidates.
	arrivals := make([]time.Time, len(candidates))
	var wg sync.WaitGroup
	for i, c := range candidates {
		wg.Add(1)
		go func(i int, runRef string) {
			defer wg.Done()
			t, _ := findStopTimeInPattern(client, runRef, stopSandownPark, arriveBy, realtime)
			arrivals[i] = t
		}(i, c.dep.RunRef)
	}
	wg.Wait()

	type hit struct {
		departTH time.Time
		arriveSD time.Time
		runRef   string
	}
	var hits []hit
	for i, c := range candidates {
		if arrivals[i].IsZero() || arrivals[i].After(arriveBy) {
			continue
		}
		hits = append(hits, hit{c.departTH, arrivals[i], c.dep.RunRef})
	}

	if len(hits) == 0 {
		return nil, fmt.Errorf("no train from Town Hall to Sandown Park arriving by %s", arriveBy.Format("15:04"))
	}

	sort.Slice(hits, func(i, j int) bool { return hits[i].departTH.After(hits[j].departTH) })
	if len(hits) > 3 {
		hits = hits[:3]
	}

	disruptions := activeDisruptions(client, arriveBy, routeCranbourne, routePakenham)
	results := make([]JourneyResult, len(hits))
	for i, h := range hits {
		results[i] = JourneyResult{
			Origin:      "Town Hall",
			Destination: "Sandown Park",
			DepartAt:    h.departTH.In(melbourneTZ).Format("15:04"),
			ArriveAt:    h.arriveSD.In(melbourneTZ).Format("15:04"),
			TrackRunRef: h.runRef,
		}
	}
	results[0].Disruptions = disruptions
	return results, nil
}

// frankPattern holds information extracted from a single GetPattern call for a Frankston train.
type frankPattern struct {
	arrivalAt time.Time
	express   bool
}

// fetchFrankPatterns fetches patterns for a list of Frankston departures in parallel.
// fromStop/toStop are the segment endpoints for the express check;
// targetStop is the stop whose arrival time is returned.
// fullStops is the intermediate-stop count for a stopping-all-stations service.
func fetchFrankPatterns(client *PTVClient, deps []Departure, fromStop, toStop, targetStop, fullStops int, refDate time.Time, realtime bool) []frankPattern {
	results := make([]frankPattern, len(deps))
	var wg sync.WaitGroup
	for i, dep := range deps {
		wg.Add(1)
		go func(i int, runRef string) {
			defer wg.Done()
			stops, err := client.GetPattern(runRef)
			if err != nil {
				return
			}
			fromIdx := -1
			for k, s := range stops {
				if s.StopID == fromStop && fromIdx < 0 {
					fromIdx = k
				}
				if s.StopID == toStop && fromIdx >= 0 {
					results[i].express = k-fromIdx-1 < fullStops
				}
				if s.StopID == targetStop {
					t, err := parseUTC(s.EffectiveTime(realtime))
					if err == nil {
						results[i].arrivalAt = rebaseToDate(t, refDate)
					}
				}
			}
		}(i, dep.RunRef)
	}
	wg.Wait()
	return results
}

// fetchStopTimes fetches the arrival time at targetStop for a list of departures in parallel.
func fetchStopTimes(client *PTVClient, deps []Departure, targetStop int, refDate time.Time, realtime bool) []time.Time {
	results := make([]time.Time, len(deps))
	var wg sync.WaitGroup
	for i, dep := range deps {
		wg.Add(1)
		go func(i int, runRef string) {
			defer wg.Done()
			t, _ := findStopTimeInPattern(client, runRef, targetStop, refDate, realtime)
			results[i] = t
		}(i, dep.RunRef)
	}
	wg.Wait()
	return results
}

// planMentone finds up to 3 latest TH→Mentone journeys arriving by arriveBy.
// Each option has a different Frankston connection at Caulfield.
func planMentone(client *PTVClient, arriveBy, windowStart time.Time, realtime bool) ([]JourneyResult, error) {
	// Leg 2: Frankston from Caulfield → Mentone.
	frankDeps, err := client.GetDepartures(stopCaulfield, routeFrankston, dirFrankston, 20, windowStart)
	if err != nil {
		return nil, fmt.Errorf("departures from Caulfield (Frankston): %w", err)
	}

	// Leg 1: C/P from TH → Caulfield.
	var thDeps []Departure
	seen := map[string]bool{}
	for _, c := range []struct{ route, dir int }{{routeCranbourne, dirCranbourne}, {routePakenham, dirPakenham}} {
		deps, _ := client.GetDepartures(stopTownHall, c.route, c.dir, 20, windowStart)
		for _, d := range deps {
			if !seen[d.RunRef] {
				seen[d.RunRef] = true
				thDeps = append(thDeps, d)
			}
		}
	}

	// Parallel-fetch in two batches.
	var frankPats []frankPattern
	var thCaulfArrs []time.Time
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		frankPats = fetchFrankPatterns(client, frankDeps,
			stopCaulfield, stopMentone, stopMentone,
			frankOutboundFullStops, arriveBy, realtime)
	}()
	wg.Add(1)
	go func() {
		defer wg.Done()
		thCaulfArrs = fetchStopTimes(client, thDeps, stopCaulfield, arriveBy, realtime)
	}()
	wg.Wait()

	// Build TH leg pairs sorted by departure DESC (latest first for arrive_by mode).
	type thLeg struct {
		runRef          string
		departTH        time.Time
		arriveCaulfield time.Time
	}
	var thLegs []thLeg
	for i, dep := range thDeps {
		if thCaulfArrs[i].IsZero() {
			continue
		}
		t, err := parseUTC(dep.EffectiveTime(realtime))
		if err != nil {
			continue
		}
		t = rebaseToDate(t, arriveBy)
		if t.After(arriveBy) {
			continue
		}
		thLegs = append(thLegs, thLeg{dep.RunRef, t, thCaulfArrs[i]})
	}
	sort.Slice(thLegs, func(i, j int) bool { return thLegs[i].departTH.After(thLegs[j].departTH) })

	type option struct {
		departTH        time.Time
		arriveCaulfield time.Time
		departCaulfield time.Time
		arriveMentone   time.Time
		express         bool
		frankRunRef     string
		thRunRef        string
	}
	var options []option

	for i, frankDep := range frankDeps {
		fp := frankPats[i]
		if fp.arrivalAt.IsZero() || fp.arrivalAt.After(arriveBy) {
			continue
		}
		t, err := parseUTC(frankDep.EffectiveTime(realtime))
		if err != nil {
			continue
		}
		departCaulfield := rebaseToDate(t, arriveBy)
		if departCaulfield.After(arriveBy) {
			continue
		}
		connectDeadline := departCaulfield.Add(-2 * time.Minute)

		// Find the latest TH train arriving at Caulfield in time (thLegs sorted latest first).
		for k := range thLegs {
			if thLegs[k].arriveCaulfield.After(connectDeadline) {
				continue
			}
			options = append(options, option{
				departTH:        thLegs[k].departTH,
				arriveCaulfield: thLegs[k].arriveCaulfield,
				departCaulfield: departCaulfield,
				arriveMentone:   fp.arrivalAt,
				express:         fp.express,
				frankRunRef:     frankDep.RunRef,
				thRunRef:        thLegs[k].runRef,
			})
			break
		}
	}

	if len(options) == 0 {
		return nil, fmt.Errorf("no train from Town Hall to Mentone arriving by %s", arriveBy.Format("15:04"))
	}

	// Sort by TH departure DESC (latest = most flexibility at origin), take top 3.
	sort.Slice(options, func(i, j int) bool { return options[i].departTH.After(options[j].departTH) })
	if len(options) > 3 {
		options = options[:3]
	}

	disruptions := activeDisruptions(client, arriveBy, routeCranbourne, routePakenham, routeFrankston)
	results := make([]JourneyResult, len(options))
	for i, o := range options {
		results[i] = JourneyResult{
			Origin:      "Town Hall",
			Destination: "Mentone",
			DepartAt:    o.departTH.In(melbourneTZ).Format("15:04"),
			Express:     o.express,
			Transfer: &Transfer{
				Station:         "Caulfield",
				ArriveCaulfield: o.arriveCaulfield.In(melbourneTZ).Format("15:04"),
				Line:            "Frankston",
				DepartCaulfield: o.departCaulfield.In(melbourneTZ).Format("15:04"),
			},
			ArriveAt:    o.arriveMentone.In(melbourneTZ).Format("15:04"),
			TrackRunRef: o.frankRunRef,
			Leg1RunRef:  o.thRunRef,
		}
	}
	results[0].Disruptions = disruptions
	return results, nil
}

// findStopTimeInPattern fetches the run pattern and returns the scheduled/estimated time
// for the given stop_id, rebased to journeyDate in Melbourne time.
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

// PlanReturnJourney finds up to 3 earliest journeys from origin to Town Hall departing at or after departAt.
// departAt is in Melbourne local time, format "15:04".
// date is optional (format "2006-01-02"); defaults to today in Melbourne time if empty.
func PlanReturnJourney(client *PTVClient, from, departAtStr, date string, realtime bool) ([]JourneyResult, error) {
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

// planReturnSandownPark finds up to 3 earliest C/P trains from Sandown Park to Town Hall from departAt.
func planReturnSandownPark(client *PTVClient, departAt time.Time, realtime bool) ([]JourneyResult, error) {
	var allDeps []Departure
	seen := map[string]bool{}
	for _, c := range []struct{ route, dir int }{{routeCranbourne, dirCity}, {routePakenham, dirCity}} {
		deps, err := client.GetDepartures(stopSandownPark, c.route, c.dir, 15, departAt)
		if err != nil {
			return nil, fmt.Errorf("departures from Sandown Park: %w", err)
		}
		for _, d := range deps {
			if !seen[d.RunRef] {
				seen[d.RunRef] = true
				allDeps = append(allDeps, d)
			}
		}
	}

	type candidate struct {
		dep      Departure
		departSD time.Time
	}
	var candidates []candidate
	for _, dep := range allDeps {
		t, err := parseUTC(dep.EffectiveTime(realtime))
		if err != nil {
			continue
		}
		t = rebaseToDate(t, departAt)
		if t.Before(departAt) {
			continue
		}
		candidates = append(candidates, candidate{dep, t})
	}

	arrivals := fetchStopTimes(client, func() []Departure {
		ds := make([]Departure, len(candidates))
		for i, c := range candidates {
			ds[i] = c.dep
		}
		return ds
	}(), stopTownHall, departAt, realtime)

	type hit struct {
		departSD time.Time
		arriveTH time.Time
		runRef   string
	}
	var hits []hit
	for i, c := range candidates {
		if arrivals[i].IsZero() {
			continue
		}
		hits = append(hits, hit{c.departSD, arrivals[i], c.dep.RunRef})
	}

	if len(hits) == 0 {
		return nil, fmt.Errorf("no train from Sandown Park to Town Hall departing after %s", departAt.Format("15:04"))
	}

	sort.Slice(hits, func(i, j int) bool { return hits[i].departSD.Before(hits[j].departSD) })
	if len(hits) > 3 {
		hits = hits[:3]
	}

	disruptions := activeDisruptions(client, departAt, routeCranbourne, routePakenham)
	results := make([]JourneyResult, len(hits))
	for i, h := range hits {
		results[i] = JourneyResult{
			Origin:      "Sandown Park",
			Destination: "Town Hall",
			DepartAt:    h.departSD.In(melbourneTZ).Format("15:04"),
			ArriveAt:    h.arriveTH.In(melbourneTZ).Format("15:04"),
			TrackRunRef: h.runRef,
		}
	}
	results[0].Disruptions = disruptions
	return results, nil
}

// planReturnMentone finds up to 3 earliest Mentone→Town Hall journeys from departAt.
// Deduplicates by C/P connection at Caulfield (same connection = same TH arrival).
func planReturnMentone(client *PTVClient, departAt time.Time, realtime bool) ([]JourneyResult, error) {
	// Leg 1: Frankston from Mentone → Caulfield.
	frankDeps, err := client.GetDepartures(stopMentone, routeFrankston, dirCity, 15, departAt)
	if err != nil {
		return nil, fmt.Errorf("departures from Mentone (Frankston): %w", err)
	}

	// Leg 2: C/P from Caulfield → Town Hall.
	var cpDeps []Departure
	seenCP := map[string]bool{}
	for _, c := range []struct{ route, dir int }{{routeCranbourne, dirCity}, {routePakenham, dirCity}} {
		deps, _ := client.GetDepartures(stopCaulfield, c.route, c.dir, 20, departAt)
		for _, d := range deps {
			if !seenCP[d.RunRef] {
				seenCP[d.RunRef] = true
				cpDeps = append(cpDeps, d)
			}
		}
	}

	// Parallel-fetch in two batches.
	var frankPats []frankPattern
	var cpTHArrs []time.Time
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		// fetchFrankPatterns: extract Caulfield arrival + express flag from each Frankston pattern.
		frankPats = fetchFrankPatterns(client, frankDeps,
			stopMentone, stopCaulfield, stopCaulfield,
			frankInboundFullStops, departAt, realtime)
	}()
	wg.Add(1)
	go func() {
		defer wg.Done()
		cpTHArrs = fetchStopTimes(client, cpDeps, stopTownHall, departAt, realtime)
	}()
	wg.Wait()

	// Parse C/P departure times from Caulfield and sort by time ASC.
	type cpLeg struct {
		runRef          string
		departCaulfield time.Time
		arriveTH        time.Time
	}
	var cpLegs []cpLeg
	for i, dep := range cpDeps {
		if cpTHArrs[i].IsZero() {
			continue
		}
		t, err := parseUTC(dep.EffectiveTime(realtime))
		if err != nil {
			continue
		}
		t = rebaseToDate(t, departAt)
		cpLegs = append(cpLegs, cpLeg{dep.RunRef, t, cpTHArrs[i]})
	}
	sort.Slice(cpLegs, func(i, j int) bool { return cpLegs[i].departCaulfield.Before(cpLegs[j].departCaulfield) })

	disruptions := activeDisruptions(client, departAt, routeCranbourne, routePakenham, routeFrankston)
	usedCP := map[string]bool{}
	var results []JourneyResult

	for i, dep := range frankDeps {
		if len(results) >= 3 {
			break
		}
		fp := frankPats[i]
		if fp.arrivalAt.IsZero() {
			continue
		}
		t, err := parseUTC(dep.EffectiveTime(realtime))
		if err != nil {
			continue
		}
		departMentone := rebaseToDate(t, departAt)
		if departMentone.Before(departAt) {
			continue
		}
		connectAfter := fp.arrivalAt.Add(2 * time.Minute)

		for _, cp := range cpLegs {
			if usedCP[cp.runRef] {
				continue
			}
			if cp.departCaulfield.Before(connectAfter) {
				continue
			}
			usedCP[cp.runRef] = true
			r := JourneyResult{
				Origin:      "Mentone",
				Destination: "Town Hall",
				DepartAt:    departMentone.In(melbourneTZ).Format("15:04"),
				Express:     fp.express,
				Transfer: &Transfer{
					Station:         "Caulfield",
					ArriveCaulfield: fp.arrivalAt.In(melbourneTZ).Format("15:04"),
					Line:            "Cranbourne / Pakenham",
					DepartCaulfield: cp.departCaulfield.In(melbourneTZ).Format("15:04"),
				},
				ArriveAt:    cp.arriveTH.In(melbourneTZ).Format("15:04"),
				TrackRunRef: cp.runRef,
				Leg1RunRef:  dep.RunRef,
			}
			if len(results) == 0 {
				r.Disruptions = disruptions
			}
			results = append(results, r)
			break
		}
	}

	if len(results) == 0 {
		return nil, fmt.Errorf("no Frankston train from Mentone departing after %s", departAt.Format("15:04"))
	}
	return results, nil
}

// PlanOutboundJourney finds up to 3 earliest TH→destination journeys departing at or after departAt.
func PlanOutboundJourney(client *PTVClient, destination, departAtStr, date string, realtime bool) ([]JourneyResult, error) {
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

// planOutboundSandownPark finds up to 3 earliest direct TH→Sandown Park trains from departAt.
func planOutboundSandownPark(client *PTVClient, departAt time.Time, realtime bool) ([]JourneyResult, error) {
	var allDeps []Departure
	seen := map[string]bool{}
	for _, c := range []struct{ route, dir int }{{routeCranbourne, dirCranbourne}, {routePakenham, dirPakenham}} {
		deps, err := client.GetDepartures(stopTownHall, c.route, c.dir, 15, departAt)
		if err != nil {
			return nil, fmt.Errorf("departures from Town Hall: %w", err)
		}
		for _, d := range deps {
			if !seen[d.RunRef] {
				seen[d.RunRef] = true
				allDeps = append(allDeps, d)
			}
		}
	}

	type candidate struct {
		dep      Departure
		departTH time.Time
	}
	var candidates []candidate
	for _, dep := range allDeps {
		t, err := parseUTC(dep.EffectiveTime(realtime))
		if err != nil {
			continue
		}
		t = rebaseToDate(t, departAt)
		if t.Before(departAt) {
			continue
		}
		candidates = append(candidates, candidate{dep, t})
	}

	arrivals := fetchStopTimes(client, func() []Departure {
		ds := make([]Departure, len(candidates))
		for i, c := range candidates {
			ds[i] = c.dep
		}
		return ds
	}(), stopSandownPark, departAt, realtime)

	type hit struct {
		departTH time.Time
		arriveSD time.Time
		runRef   string
	}
	var hits []hit
	for i, c := range candidates {
		if arrivals[i].IsZero() {
			continue
		}
		hits = append(hits, hit{c.departTH, arrivals[i], c.dep.RunRef})
	}

	if len(hits) == 0 {
		return nil, fmt.Errorf("no train from Town Hall to Sandown Park departing after %s", departAt.Format("15:04"))
	}

	sort.Slice(hits, func(i, j int) bool { return hits[i].departTH.Before(hits[j].departTH) })
	if len(hits) > 3 {
		hits = hits[:3]
	}

	disruptions := activeDisruptions(client, departAt, routeCranbourne, routePakenham)
	results := make([]JourneyResult, len(hits))
	for i, h := range hits {
		results[i] = JourneyResult{
			Origin:      "Town Hall",
			Destination: "Sandown Park",
			DepartAt:    h.departTH.In(melbourneTZ).Format("15:04"),
			ArriveAt:    h.arriveSD.In(melbourneTZ).Format("15:04"),
			TrackRunRef: h.runRef,
		}
	}
	results[0].Disruptions = disruptions
	return results, nil
}

// planOutboundMentone finds up to 3 earliest TH→Mentone journeys from departAt.
// Deduplicates by Frankston connection at Caulfield.
func planOutboundMentone(client *PTVClient, departAt time.Time, realtime bool) ([]JourneyResult, error) {
	// Leg 1: C/P from TH → Caulfield.
	var thDeps []Departure
	seen := map[string]bool{}
	for _, c := range []struct{ route, dir int }{{routeCranbourne, dirCranbourne}, {routePakenham, dirPakenham}} {
		deps, err := client.GetDepartures(stopTownHall, c.route, c.dir, 15, departAt)
		if err != nil {
			return nil, fmt.Errorf("departures from Town Hall: %w", err)
		}
		for _, d := range deps {
			if !seen[d.RunRef] {
				seen[d.RunRef] = true
				thDeps = append(thDeps, d)
			}
		}
	}

	// Leg 2: Frankston from Caulfield → Mentone.
	frankDeps, err := client.GetDepartures(stopCaulfield, routeFrankston, dirFrankston, 20, departAt)
	if err != nil {
		return nil, fmt.Errorf("departures from Caulfield (Frankston): %w", err)
	}

	// Parallel-fetch in two batches.
	var thCaulfArrs []time.Time
	var frankPats []frankPattern
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		thCaulfArrs = fetchStopTimes(client, thDeps, stopCaulfield, departAt, realtime)
	}()
	wg.Add(1)
	go func() {
		defer wg.Done()
		frankPats = fetchFrankPatterns(client, frankDeps,
			stopCaulfield, stopMentone, stopMentone,
			frankOutboundFullStops, departAt, realtime)
	}()
	wg.Wait()

	// Build TH leg pairs sorted by departure ASC (earliest first for depart_at mode).
	type thLeg struct {
		runRef          string
		departTH        time.Time
		arriveCaulfield time.Time
	}
	var thLegs []thLeg
	for i, dep := range thDeps {
		if thCaulfArrs[i].IsZero() {
			continue
		}
		t, err := parseUTC(dep.EffectiveTime(realtime))
		if err != nil {
			continue
		}
		t = rebaseToDate(t, departAt)
		if t.Before(departAt) {
			continue
		}
		thLegs = append(thLegs, thLeg{dep.RunRef, t, thCaulfArrs[i]})
	}
	sort.Slice(thLegs, func(i, j int) bool { return thLegs[i].departTH.Before(thLegs[j].departTH) })

	// Build Frankston leg pairs sorted by departure ASC.
	type frankLeg struct {
		runRef          string
		departCaulfield time.Time
		arriveMentone   time.Time
		express         bool
	}
	var frankLegs []frankLeg
	for i, dep := range frankDeps {
		fp := frankPats[i]
		if fp.arrivalAt.IsZero() {
			continue
		}
		t, err := parseUTC(dep.EffectiveTime(realtime))
		if err != nil {
			continue
		}
		t = rebaseToDate(t, departAt)
		frankLegs = append(frankLegs, frankLeg{dep.RunRef, t, fp.arrivalAt, fp.express})
	}
	sort.Slice(frankLegs, func(i, j int) bool { return frankLegs[i].departCaulfield.Before(frankLegs[j].departCaulfield) })

	disruptions := activeDisruptions(client, departAt, routeCranbourne, routePakenham, routeFrankston)
	usedFrank := map[string]bool{}
	var results []JourneyResult

	for _, th := range thLegs {
		if len(results) >= 3 {
			break
		}
		connectAfter := th.arriveCaulfield.Add(2 * time.Minute)
		for _, fl := range frankLegs {
			if usedFrank[fl.runRef] {
				continue
			}
			if fl.departCaulfield.Before(connectAfter) {
				continue
			}
			usedFrank[fl.runRef] = true
			r := JourneyResult{
				Origin:      "Town Hall",
				Destination: "Mentone",
				DepartAt:    th.departTH.In(melbourneTZ).Format("15:04"),
				Express:     fl.express,
				Transfer: &Transfer{
					Station:         "Caulfield",
					ArriveCaulfield: th.arriveCaulfield.In(melbourneTZ).Format("15:04"),
					Line:            "Frankston",
					DepartCaulfield: fl.departCaulfield.In(melbourneTZ).Format("15:04"),
				},
				ArriveAt:    fl.arriveMentone.In(melbourneTZ).Format("15:04"),
				TrackRunRef: fl.runRef,
				Leg1RunRef:  th.runRef,
			}
			if len(results) == 0 {
				r.Disruptions = disruptions
			}
			results = append(results, r)
			break
		}
	}

	if len(results) == 0 {
		return nil, fmt.Errorf("no train from Town Hall to Mentone departing after %s", departAt.Format("15:04"))
	}
	return results, nil
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
			from, err := time.Parse(time.RFC3339, d.FromDate)
			if err != nil || journeyDate.Before(from) {
				continue
			}
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
// time on journeyDate.
func rebaseToDate(apiTime, journeyDate time.Time) time.Time {
	local := apiTime.In(melbourneTZ)
	base := journeyDate.In(melbourneTZ)
	return time.Date(base.Year(), base.Month(), base.Day(),
		local.Hour(), local.Minute(), local.Second(), 0, melbourneTZ)
}

func parseUTC(s string) (time.Time, error) {
	return time.Parse(time.RFC3339, s)
}

// countStopsBetween returns the number of intermediate stops between fromStop and toStop
// in a run's pattern. Returns -1 if either stop is not found.
func countStopsBetween(client *PTVClient, runRef string, fromStop, toStop int) int {
	stops, err := client.GetPattern(runRef)
	if err != nil {
		return -1
	}
	fromIdx := -1
	for i, s := range stops {
		if s.StopID == fromStop && fromIdx < 0 {
			fromIdx = i
		} else if s.StopID == toStop && fromIdx >= 0 {
			return i - fromIdx - 1
		}
	}
	return -1
}
