package main

import (
	"context"
	"fmt"
	"log"
	"strconv"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
)

type collectRoute struct {
	name         string
	originStop   int
	destStop     int
	routes       []routeDir
	transferStop int        // 0 = direct; non-zero = transfer at this stop
	leg2Routes   []routeDir // required when transferStop != 0
}

var collectRoutes = []collectRoute{
	{
		name:       "sandown_park_inbound",
		originStop: stopSandownPark,
		destStop:   stopTownHall,
		routes:     []routeDir{{routeCranbourne, dirCity}, {routePakenham, dirCity}},
	},
	{
		name:       "sandown_park_outbound",
		originStop: stopTownHall,
		destStop:   stopSandownPark,
		routes:     []routeDir{{routeCranbourne, dirCranbourne}, {routePakenham, dirPakenham}},
	},
	{
		name:         "mentone_inbound",
		originStop:   stopMentone,
		destStop:     stopTownHall,
		routes:       []routeDir{{routeFrankston, dirCity}},
		transferStop: stopCaulfield,
		leg2Routes:   []routeDir{{routeCranbourne, dirCity}, {routePakenham, dirCity}},
	},
	{
		name:         "mentone_outbound",
		originStop:   stopTownHall,
		destStop:     stopMentone,
		routes:       []routeDir{{routeCranbourne, dirCranbourne}, {routePakenham, dirPakenham}},
		transferStop: stopCaulfield,
		leg2Routes:   []routeDir{{routeFrankston, dirFrankston}},
	},
}

// Collect fetches departures in the next 90 minutes from each origin stop,
// and persists any train that has live estimated data into DynamoDB.
// It also refreshes arrival estimates for runs that are currently in transit.
func Collect(ctx context.Context, ptv *PTVClient, db *dynamodb.Client, table string) error {
	now := time.Now().In(melbourneTZ)

	updateInFlightArrivals(ctx, ptv, db, table, now)

	date := now.Format("2006-01-02")
	windowTo := now.Add(90 * time.Minute)
	ttl := now.Add(30 * 24 * time.Hour).Unix()

	var errs []error

	for _, cr := range collectRoutes {
		var stored int
		var err error
		if cr.transferStop == 0 {
			stored, err = storeDirect(ctx, ptv, db, table, cr, date, now, windowTo, ttl)
		} else {
			stored, err = storeTransfer(ctx, ptv, db, table, cr, date, now, windowTo, ttl)
		}
		if err != nil {
			errs = append(errs, err)
		}
		if stored > 0 {
			log.Printf("collector: stored %d records for %s", stored, cr.name)
		}
	}

	log.Printf("collector: done for %s at %s", date, now.Format("15:04"))

	if len(errs) > 0 {
		return errs[0]
	}
	return nil
}

func storeDirect(ctx context.Context, ptv *PTVClient, db *dynamodb.Client, table string,
	cr collectRoute, date string, now, windowTo time.Time, ttl int64) (int, error) {

	deps := fetchDepsInWindow(ptv, cr.originStop, cr.routes, now, windowTo, now)
	arrivals := parallelStopTimes(ptv, deps, cr.destStop, now)

	stored := 0
	for i, dep := range deps {
		if !dep.hasEst {
			continue
		}
		arr := arrivals[i]
		if arr == nil || arr.Scheduled.IsZero() {
			continue
		}
		schedMin := roundMinutes(arr.Scheduled.Sub(dep.departSch))
		if schedMin <= 0 {
			continue
		}

		item := baseItem(cr.name, date, dep, arr.Scheduled, schedMin, ttl)
		if arr.HasEstimate {
			estMin := roundMinutes(arr.Estimated.Sub(dep.departEst))
			item["arrive_actual"] = strAttr(arr.Estimated.In(melbourneTZ).Format("15:04"))
			item["travel_min_actual"] = numAttr(estMin)
		}

		if _, err := db.PutItem(ctx, &dynamodb.PutItemInput{
			TableName: aws.String(table),
			Item:      item,
		}); err != nil {
			return stored, fmt.Errorf("putItem %s %s: %w", cr.name, dep.runRef, err)
		}
		stored++
	}
	return stored, nil
}

func storeTransfer(ctx context.Context, ptv *PTVClient, db *dynamodb.Client, table string,
	cr collectRoute, date string, now, windowTo time.Time, ttl int64) (int, error) {

	// Leg 1: origin → transfer stop
	leg1Deps := fetchDepsInWindow(ptv, cr.originStop, cr.routes, now, windowTo, now)
	transArrivals := parallelStopTimes(ptv, leg1Deps, cr.transferStop, now)

	// Leg 2: transfer stop → dest (extended window to catch connections)
	leg2Deps := fetchDepsInWindow(ptv, cr.transferStop, cr.leg2Routes, now, windowTo.Add(time.Hour), now)
	destArrivals := parallelStopTimes(ptv, leg2Deps, cr.destStop, now)

	stored := 0
	for i, dep := range leg1Deps {
		if !dep.hasEst {
			continue
		}
		transArr := transArrivals[i]
		if transArr == nil || transArr.Scheduled.IsZero() {
			continue
		}

		// First leg 2 train departing ≥2 min after scheduled transfer arrival
		connectAfter := transArr.Scheduled.Add(2 * time.Minute)
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

		schedMin := roundMinutes(destArr.Scheduled.Sub(dep.departSch))
		if schedMin <= 0 {
			continue
		}

		item := baseItem(cr.name, date, dep, destArr.Scheduled, schedMin, ttl)
		item["connect_run_ref"] = strAttr(conn.runRef)

		if conn.hasEst && destArr.HasEstimate {
			estConnectAfter := transArr.Estimated.Add(2 * time.Minute)
			if !conn.departEst.Before(estConnectAfter) {
				estMin := roundMinutes(destArr.Estimated.Sub(dep.departEst))
				item["arrive_actual"] = strAttr(destArr.Estimated.In(melbourneTZ).Format("15:04"))
				item["travel_min_actual"] = numAttr(estMin)
			}
		}

		if _, err := db.PutItem(ctx, &dynamodb.PutItemInput{
			TableName: aws.String(table),
			Item:      item,
		}); err != nil {
			return stored, fmt.Errorf("putItem %s %s: %w", cr.name, dep.runRef, err)
		}
		stored++
	}
	return stored, nil
}

// baseItem builds the common DynamoDB attributes for a stored departure.
func baseItem(routeName, date string, dep depJob, arrSch time.Time, schedMin int, ttl int64) map[string]types.AttributeValue {
	return map[string]types.AttributeValue{
		"pk":                   strAttr(date + "#" + routeName),
		"sk":                   strAttr(dep.runRef),
		"route":                strAttr(routeName),
		"date":                 strAttr(date),
		"depart_scheduled":     strAttr(dep.departSch.In(melbourneTZ).Format("15:04")),
		"depart_actual":        strAttr(dep.departEst.In(melbourneTZ).Format("15:04")),
		"arrive_scheduled":     strAttr(arrSch.In(melbourneTZ).Format("15:04")),
		"travel_min_scheduled": numAttr(schedMin),
		"delay_min":            numAttr(roundMinutes(dep.departEst.Sub(dep.departSch))),
		"collected_at":         strAttr(time.Now().UTC().Format(time.RFC3339)),
		"ttl":                  &types.AttributeValueMemberN{Value: strconv.FormatInt(ttl, 10)},
	}
}

// updateInFlightArrivals refreshes arrive_actual and travel_min_actual for runs
// that have already departed but haven't reached their destination yet.
func updateInFlightArrivals(ctx context.Context, ptv *PTVClient, db *dynamodb.Client, table string, now time.Time) {
	date := now.Format("2006-01-02")
	var wg sync.WaitGroup
	for _, cr := range collectRoutes {
		cr := cr
		wg.Add(1)
		go func() {
			defer wg.Done()
			if n := refreshInFlightRoute(ctx, ptv, db, table, cr, date, now); n > 0 {
				log.Printf("in-flight: updated %d arrivals for %s", n, cr.name)
			}
		}()
	}
	wg.Wait()
}

func refreshInFlightRoute(ctx context.Context, ptv *PTVClient, db *dynamodb.Client, table string,
	cr collectRoute, date string, now time.Time) int {

	out, err := db.Query(ctx, &dynamodb.QueryInput{
		TableName:              aws.String(table),
		KeyConditionExpression: aws.String("pk = :pk"),
		ExpressionAttributeValues: map[string]types.AttributeValue{
			":pk": strAttr(date + "#" + cr.name),
		},
	})
	if err != nil {
		log.Printf("in-flight query %s: %v", cr.name, err)
		return 0
	}

	type inFlightJob struct {
		runRef         string
		depActual      string
		connectRunRef  string // non-empty for transfer routes
	}
	var jobs []inFlightJob
	for _, item := range out.Items {
		depTime := parseHHMM(strVal(item, "depart_scheduled"), date)
		arrTime := parseHHMM(strVal(item, "arrive_scheduled"), date)
		if now.Before(depTime) || now.After(arrTime.Add(30*time.Minute)) {
			continue
		}
		jobs = append(jobs, inFlightJob{
			runRef:        strVal(item, "sk"),
			depActual:     strVal(item, "depart_actual"),
			connectRunRef: strVal(item, "connect_run_ref"),
		})
	}
	if len(jobs) == 0 {
		return 0
	}

	type fetchResult struct {
		runRef    string
		depActual string
		arr       *StopTimes
	}
	fetched := make([]fetchResult, len(jobs))
	var wg sync.WaitGroup
	for i, j := range jobs {
		wg.Add(1)
		go func(i int, j inFlightJob) {
			defer wg.Done()
			// For transfer routes, the destination stop is in the connecting run's pattern.
			lookupRef := j.runRef
			if j.connectRunRef != "" {
				lookupRef = j.connectRunRef
			}
			arr, _ := findBothStopTimesInPattern(ptv, lookupRef, cr.destStop, now)
			fetched[i] = fetchResult{runRef: j.runRef, depActual: j.depActual, arr: arr}
		}(i, j)
	}
	wg.Wait()

	updated := 0
	for _, r := range fetched {
		if r.arr == nil || !r.arr.HasEstimate {
			continue
		}
		depTime := parseHHMM(r.depActual, date)
		travelMin := roundMinutes(r.arr.Estimated.Sub(depTime))
		if travelMin <= 0 {
			continue
		}
		_, err := db.UpdateItem(ctx, &dynamodb.UpdateItemInput{
			TableName: aws.String(table),
			Key: map[string]types.AttributeValue{
				"pk": strAttr(date + "#" + cr.name),
				"sk": strAttr(r.runRef),
			},
			UpdateExpression: aws.String("SET arrive_actual = :aa, travel_min_actual = :ta"),
			ExpressionAttributeValues: map[string]types.AttributeValue{
				":aa": strAttr(r.arr.Estimated.In(melbourneTZ).Format("15:04")),
				":ta": numAttr(travelMin),
			},
		})
		if err != nil {
			log.Printf("in-flight update %s %s: %v", cr.name, r.runRef, err)
		} else {
			updated++
		}
	}
	return updated
}

func parseHHMM(hhmm, date string) time.Time {
	t, _ := time.ParseInLocation("2006-01-02 15:04", date+" "+hhmm, melbourneTZ)
	return t
}

func strAttr(v string) types.AttributeValue {
	return &types.AttributeValueMemberS{Value: v}
}

func numAttr(v int) types.AttributeValue {
	return &types.AttributeValueMemberN{Value: strconv.Itoa(v)}
}
