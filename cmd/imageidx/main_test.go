package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"
)

func TestRunOnce(t *testing.T) {
	var output bytes.Buffer
	if err := runOnce(&output); err != nil {
		t.Fatal(err)
	}
	var response searchResponse
	if err := json.Unmarshal(output.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(response.ImageIDs, []uint64{100, 200}) {
		t.Fatalf("image IDs = %v", response.ImageIDs)
	}
}

func TestSearchHandler(t *testing.T) {
	service, err := newDemoService()
	if err != nil {
		t.Fatal(err)
	}
	defer service.Close()

	path := fmt.Sprintf("/search?start=%d&end=%d&cell=%d", demoQuantum, 3*demoQuantum, service.demoCell)
	request := httptest.NewRequest(http.MethodGet, path, nil)
	response := httptest.NewRecorder()
	service.handler().ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", response.Code, response.Body.String())
	}
	var body searchResponse
	if err = json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(body.ImageIDs, []uint64{100, 200}) {
		t.Fatalf("image IDs = %v", body.ImageIDs)
	}
}
