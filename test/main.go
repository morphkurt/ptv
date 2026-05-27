package main

import (
	"crypto/hmac"
	"crypto/sha1"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"strings"
)

const (
	baseURL  = "https://timetableapi.ptv.vic.gov.au"
	devID    = "1000080"
	apiKey   = "786416fc-aa6c-11e3-8bed-0263a9d0b8a0"
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

func main() {
	url := sign("/v3/routes?route_types=0") // route_type 0 = train
	fmt.Println("Requesting:", url)

	resp, err := http.Get(url)
	if err != nil {
		fmt.Println("Error:", err)
		return
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	fmt.Printf("Status: %d\n", resp.StatusCode)
	fmt.Printf("Body (first 500 chars): %s\n", string(body[:min(500, len(body))]))
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
