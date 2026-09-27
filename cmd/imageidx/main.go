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

	common "github.com/evanoberholster/imageIdx/common"
	"github.com/evanoberholster/imageIdx/index/bitmapidx"

	"github.com/evanoberholster/imageIdx/index/timeidx"
	"github.com/evanoberholster/imageIdx/search"
)

const demoQuantum = uint64(1) << timeidx.TimestampShift

type searchRequest struct {
	Start   uint64   `json:"start"`
	End     uint64   `json:"end"`
	CellIDs []uint64 `json:"cell_ids"`
}

type searchResponse struct {
	ImageIDs []common.ImageID `json:"image_ids"`
}

type demoService struct {
	namespaces   *bitmapidx.Store
	favorites    *bitmapidx.Index
	namespaceDir string
	times        *timeidx.Index
	geo          *bitmapidx.Index
	canonical    map[common.ImageID]uint64
	demoCell     uint64
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
	ids, err := service.searchOnce(context.Background(), request)
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
	namespaceDir, err := os.MkdirTemp("", "imageidx-namespaces-")
	if err != nil {
		return nil, err
	}
	namespaces, err := bitmapidx.NewStore(namespaceDir, []bitmapidx.NamespaceSpec{
		{Namespace: common.NamespaceTag, Kind: common.KindBitmapEnum},
		{Namespace: common.NamespaceLocation, Kind: common.KindBitmapGeo, Geo: bitmapidx.GeoCodec{Version: 1}},
	}, bitmapidx.NamespaceConfig{}, common.DurabilityOS)
	if err != nil {
		_ = os.RemoveAll(namespaceDir)
		return nil, err
	}
	favorites, ok := namespaces.Lookup(common.NamespaceTag)
	if !ok {
		_ = namespaces.Close()
		_ = os.RemoveAll(namespaceDir)
		return nil, bitmapidx.ErrInvalidNamespace
	}
	favoritesBatch := favorites.NewBatch()
	if err := favoritesBatch.Add(
		bitmapidx.Entry{ImageID: 100, Value: bitmapidx.EnumStringValue("favorite")},
		bitmapidx.Entry{ImageID: 200, Value: bitmapidx.EnumStringValue("favorite")},
		bitmapidx.Entry{ImageID: 300, Value: bitmapidx.EnumStringValue("favorite")},
	); err != nil {
		_ = namespaces.Close()
		_ = os.RemoveAll(namespaceDir)
		return nil, err
	}
	if err := favoritesBatch.Apply(); err != nil {
		_ = namespaces.Close()
		_ = os.RemoveAll(namespaceDir)
		return nil, err
	}
	favoritesBatch.Close()
	if err := favorites.Barrier(); err != nil {
		_ = namespaces.Close()
		_ = os.RemoveAll(namespaceDir)
		return nil, err
	}
	geo, ok := namespaces.Lookup(common.NamespaceLocation)
	if !ok {
		_ = namespaces.Close()
		_ = os.RemoveAll(namespaceDir)
		return nil, bitmapidx.ErrInvalidNamespace
	}
	for _, entry := range []struct {
		id  common.ImageID
		lat float64
		lng float64
	}{
		{100, 37.7749, -122.4194}, {200, 37.7749, -122.4194},
		{300, 34.0522, -118.2437}, {400, 40.7128, -74.0060},
	} {
		key, err := bitmapidx.KeyFromLatLng(entry.lat, entry.lng)
		if err != nil {
			_ = namespaces.Close()
			_ = os.RemoveAll(namespaceDir)
			return nil, err
		}
		geoBatch := geo.NewBatch()
		if err := geoBatch.Move(bitmapidx.Move{ImageID: entry.id, To: int64(key)}); err != nil {
			_ = namespaces.Close()
			_ = os.RemoveAll(namespaceDir)
			return nil, err
		}
		if err := geoBatch.Apply(); err != nil {
			_ = namespaces.Close()
			_ = os.RemoveAll(namespaceDir)
			return nil, err
		}
		geoBatch.Close()
		if err := geo.Barrier(); err != nil {
			_ = namespaces.Close()
			_ = os.RemoveAll(namespaceDir)
			return nil, err
		}
	}

	canonical := map[common.ImageID]uint64{
		100: demoQuantum,
		200: 2 * demoQuantum,
		300: 3 * demoQuantum,
		400: 4 * demoQuantum,
	}
	times, err := timeidx.New(timeidx.Spec{
		Namespace: common.Namespace(100),
		Name:      "timestamp",
		Entries: []timeidx.Entry{
			{ImageID: 100, Timestamp: canonical[100]},
			{ImageID: 200, Timestamp: canonical[200]},
			{ImageID: 300, Timestamp: canonical[300]},
			{ImageID: 400, Timestamp: canonical[400]},
		},
	}, timeidx.Config{})
	if err != nil {
		_ = namespaces.Close()
		_ = os.RemoveAll(namespaceDir)
		return nil, err
	}

	return &demoService{
		namespaces: namespaces, favorites: favorites, namespaceDir: namespaceDir, times: times, geo: geo,
		canonical: canonical, demoCell: bitmapidx.CellIDFromLatLng(37.7749, -122.4194),
	}, nil
}

func (service *demoService) Close() error {
	var first error
	for _, closeIndex := range []func() error{service.geo.Close, service.times.Close, service.namespaces.Close} {
		if err := closeIndex(); err != nil && first == nil {
			first = err
		}
	}
	if err := os.RemoveAll(service.namespaceDir); err != nil && first == nil {
		first = err
	}
	return first
}

func (service *demoService) searchOnce(ctx context.Context, request searchRequest) ([]common.ImageID, error) {
	return search.SearchOnce(ctx, nil,
		search.AllValues(service.favorites, bitmapidx.EnumStringValue("favorite")),
		search.During(service.times, request.Start, request.End,
			search.ExactTimestamp(func(id common.ImageID) (uint64, bool) {
				value, ok := service.canonical[id]
				return value, ok
			})),
		search.InCells(service.geo, request.CellIDs...),
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
		ids, err := service.searchOnce(request.Context(), query)
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
