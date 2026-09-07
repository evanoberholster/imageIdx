package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	imageidx "github.com/evanoberholster/imageIdx"
	"github.com/evanoberholster/imageIdx/bitmapidx"
	"github.com/evanoberholster/imageIdx/geoidx"
	"github.com/evanoberholster/imageIdx/search"
	"github.com/evanoberholster/imageIdx/timeidx"
)

const demoQuantum = uint64(1) << timeidx.TimestampShift

type searchRequest struct {
	Start   uint64   `json:"start"`
	End     uint64   `json:"end"`
	CellIDs []uint64 `json:"cell_ids"`
}

type searchResponse struct {
	ImageIDs []imageidx.ImageID `json:"image_ids"`
}

type demoService struct {
	favorites *bitmapidx.Index
	times     *timeidx.Index
	geo       *geoidx.Index
	canonical map[imageidx.ImageID]uint64
	demoCell  uint64
}

func main() {
	mode := flag.String("mode", "once", "run mode: once or server")
	address := flag.String("addr", ":8080", "HTTP listen address in server mode")
	flag.Parse()

	var err error
	switch *mode {
	case "once":
		err = runOnce(os.Stdout)
	case "server":
		ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer cancel()
		err = runServer(ctx, *address)
	default:
		err = fmt.Errorf("unknown mode %q", *mode)
	}
	if err != nil {
		log.Fatal(err)
	}
}

// runOnce demonstrates the convenience path that compiles and executes one
// cross-index query immediately.
func runOnce(output io.Writer) error {
	service, err := newDemoService()
	if err != nil {
		return err
	}
	defer service.Close()

	request := searchRequest{
		Start: demoQuantum, End: 3 * demoQuantum,
		CellIDs: []uint64{service.demoCell},
	}
	ids, err := service.searchOnce(request)
	if err != nil {
		return err
	}
	return json.NewEncoder(output).Encode(searchResponse{ImageIDs: ids})
}

// runServer demonstrates a long-lived HTTP service. Each request creates a
// validated plan from cell IDs and a half-open timestamp range, then searches
// the current immutable views of all three indexes.
func runServer(ctx context.Context, address string) error {
	service, err := newDemoService()
	if err != nil {
		return err
	}
	defer service.Close()

	server := &http.Server{
		Addr:              address,
		Handler:           service.handler(),
		ReadHeaderTimeout: 5 * time.Second,
	}
	stopped := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			_ = server.Shutdown(shutdownCtx)
		case <-stopped:
		}
	}()

	log.Printf("imageidx server listening on %s; sample: /search?start=%d&end=%d&cell=%d",
		address, demoQuantum, 3*demoQuantum, service.demoCell)
	err = server.ListenAndServe()
	close(stopped)
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

func newDemoService() (*demoService, error) {
	favorites, err := bitmapidx.New(bitmapidx.Spec{
		Key:     bitmapidx.Key{Type: bitmapidx.IndexTypeTag, ValueID: 1},
		IndexID: 1,
		IDs:     []imageidx.ImageID{100, 200, 300},
	}, bitmapidx.Config{})
	if err != nil {
		return nil, err
	}

	canonical := map[imageidx.ImageID]uint64{
		100: demoQuantum,
		200: 2 * demoQuantum,
		300: 3 * demoQuantum,
		400: 4 * demoQuantum,
	}
	times, err := timeidx.New(timeidx.Spec{
		IndexID: 2,
		Entries: []timeidx.Entry{
			{ImageID: 100, Timestamp: canonical[100]},
			{ImageID: 200, Timestamp: canonical[200]},
			{ImageID: 300, Timestamp: canonical[300]},
			{ImageID: 400, Timestamp: canonical[400]},
		},
	}, timeidx.Config{})
	if err != nil {
		_ = favorites.Close()
		return nil, err
	}

	geo, err := geoidx.New(geoidx.Spec{
		IndexID: 3,
		Entries: []geoidx.Entry{
			{ImageID: 100, Latitude: 37.7749, Longitude: -122.4194},
			{ImageID: 200, Latitude: 37.7749, Longitude: -122.4194},
			{ImageID: 300, Latitude: 34.0522, Longitude: -118.2437},
			{ImageID: 400, Latitude: 40.7128, Longitude: -74.0060},
		},
	}, geoidx.Config{})
	if err != nil {
		_ = times.Close()
		_ = favorites.Close()
		return nil, err
	}
	key, err := geoidx.KeyFromLatLng(37.7749, -122.4194)
	if err != nil {
		_ = geo.Close()
		_ = times.Close()
		_ = favorites.Close()
		return nil, err
	}
	return &demoService{
		favorites: favorites, times: times, geo: geo,
		canonical: canonical, demoCell: geoidx.DecodeS2Key(key),
	}, nil
}

func (service *demoService) Close() error {
	var first error
	for _, closeIndex := range []func() error{service.geo.Close, service.times.Close, service.favorites.Close} {
		if err := closeIndex(); err != nil && first == nil {
			first = err
		}
	}
	return first
}

func (service *demoService) searchOnce(request searchRequest) ([]imageidx.ImageID, error) {
	geoClause, err := search.WithinCells(service.geo, request.CellIDs...)
	if err != nil {
		return nil, err
	}
	return search.Search(nil,
		search.AllBitmaps(service.favorites),
		search.During(service.times, request.Start, request.End,
			search.ExactTimestamp(func(id imageidx.ImageID) (uint64, bool) {
				value, ok := service.canonical[id]
				return value, ok
			})),
		geoClause,
	)
}

func (service *demoService) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /search", func(response http.ResponseWriter, request *http.Request) {
		query, err := parseSearchRequest(request)
		if err != nil {
			http.Error(response, err.Error(), http.StatusBadRequest)
			return
		}
		ids, err := service.searchOnce(query)
		if err != nil {
			http.Error(response, err.Error(), http.StatusBadRequest)
			return
		}
		response.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(response).Encode(searchResponse{ImageIDs: ids})
	})
	return mux
}

func parseSearchRequest(request *http.Request) (searchRequest, error) {
	start, err := strconv.ParseUint(request.URL.Query().Get("start"), 10, 64)
	if err != nil {
		return searchRequest{}, fmt.Errorf("start must be a uint64: %w", err)
	}
	end, err := strconv.ParseUint(request.URL.Query().Get("end"), 10, 64)
	if err != nil || start >= end {
		return searchRequest{}, errors.New("end must be a uint64 greater than start")
	}
	values := request.URL.Query()["cell"]
	if len(values) == 0 {
		return searchRequest{}, errors.New("at least one cell query parameter is required")
	}
	cells := make([]uint64, 0, len(values))
	for _, value := range values {
		for _, token := range strings.Split(value, ",") {
			cell, parseErr := strconv.ParseUint(strings.TrimSpace(token), 0, 64)
			if parseErr != nil {
				return searchRequest{}, fmt.Errorf("cell must be a decimal or 0x-prefixed uint64: %w", parseErr)
			}
			cells = append(cells, cell)
		}
	}
	return searchRequest{Start: start, End: end, CellIDs: cells}, nil
}
