package main

import (
	"crypto/hmac"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
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
	resp, err := http.Get(sign(path))
	if err != nil {
		panic(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return body
}

func searchStop(term string) {
	path := "/v3/search/" + url.PathEscape(term) + "?route_types=0"
	body := get(path)
	var result struct {
		Stops []struct {
			StopID       int    `json:"stop_id"`
			StopName     string `json:"stop_name"`
			RouteType    int    `json:"route_type"`
			StopSuburb   string `json:"stop_suburb"`
		} `json:"stops"`
	}
	json.Unmarshal(body, &result)
	fmt.Printf("=== %s ===\n", term)
	for _, s := range result.Stops {
		fmt.Printf("  stop_id=%-6d  %s (%s)\n", s.StopID, s.StopName, s.StopSuburb)
	}
}

func main() {
	for _, station := range os.Args[1:] {
		searchStop(station)
	}
}
