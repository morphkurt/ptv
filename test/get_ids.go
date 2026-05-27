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

func main() {
	// Get all train routes
	body := get("/v3/routes?route_types=0")
	var routes struct {
		Routes []struct {
			RouteID   int    `json:"route_id"`
			RouteName string `json:"route_name"`
		} `json:"routes"`
	}
	json.Unmarshal(body, &routes)
	fmt.Println("=== Train Routes ===")
	for _, r := range routes.Routes {
		if r.RouteName == "Frankston" || r.RouteName == "Cranbourne" || r.RouteName == "Pakenham" {
			fmt.Printf("  route_id=%-4d  %s\n", r.RouteID, r.RouteName)
			// Get directions for this route
			dirBody := get(fmt.Sprintf("/v3/directions/route/%d", r.RouteID))
			var dirs struct {
				Directions []struct {
					DirectionID   int    `json:"direction_id"`
					DirectionName string `json:"direction_name"`
					RouteName     string `json:"route_name"`
				} `json:"directions"`
			}
			json.Unmarshal(dirBody, &dirs)
			for _, d := range dirs.Directions {
				fmt.Printf("    direction_id=%-4d  %s\n", d.DirectionID, d.DirectionName)
			}
		}
	}
}
