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
	"github.com/evanoberholster/imageIdx/index"
	"github.com/evanoberholster/imageIdx/index/bitmapidx"
	"github.com/evanoberholster/imageIdx/index/geoidx"
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
	store     *index.Store
	engine    *search.Engine
	executor  *search.Executor
	dir       string
	canonical map[common.ImageID]uint64
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

// runOnce builds one entity-scoped query over a unified Store.
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
// the current immutable views of all three namespaces.
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
	schema := common.NewSchema()
	tags, err := index.BitmapDef(bitmapidx.NamespaceSpec{
		Namespace: common.NamespaceTag, Kind: common.KindBitmapEnum,
	}, "tags", common.EntityImage)
	if err != nil {
		return nil, err
	}
	if err := schema.RegisterAll(tags,
		common.NamespaceDef{Namespace: common.NamespaceLocation, Name: "location", Kind: common.KindGeo, Entity: common.EntityImage},
		common.NamespaceDef{Namespace: 100, Name: "timestamp", Kind: common.KindTimestamp, Entity: common.EntityImage},
	); err != nil {
		return nil, err
	}
	dir, err := os.MkdirTemp("", "imageidx-unified-")
	if err != nil {
		return nil, err
	}
	db, err := index.Create(dir, index.Spec{Schema: schema})
	if err != nil {
		_ = os.RemoveAll(dir)
		return nil, err
	}
	service := &demoService{store: db, dir: dir}
	fail := func(err error) (*demoService, error) {
		_ = service.Close()
		return nil, err
	}
	service.engine, err = search.New(db)
	if err != nil {
		return fail(err)
	}
	service.executor, err = search.NewExecutor(search.ExecutorConfig{WorkspaceCapacity: 64})
	if err != nil {
		return fail(err)
	}
	service.canonical = map[common.ImageID]uint64{
		100: demoQuantum, 200: 2 * demoQuantum,
		300: 3 * demoQuantum, 400: 4 * demoQuantum,
	}
	for _, row := range []struct {
		id  common.ImageID
		lat float64
		lng float64
	}{
		{100, 37.7749, -122.4194}, {200, 37.7749, -122.4194},
		{300, 34.0522, -118.2437}, {400, 40.7128, -74.0060},
	} {
		key, err := geoidx.KeyFromLatLng(row.lat, row.lng)
		if err != nil {
			return fail(err)
		}
		entry := index.Entry{ValueID: row.id,
			Times: []index.TimeValue{{Namespace: 100, Timestamp: service.canonical[row.id]}},
			Geos:  []index.GeoValue{{Namespace: common.NamespaceLocation, Key: key}},
		}
		if row.id != 400 {
			entry.Bitmaps = []index.BitmapValue{{Namespace: common.NamespaceTag, Value: bitmapidx.EnumStringValue("favorite")}}
		}
		if err := db.Add(entry); err != nil {
			return fail(err)
		}
	}
	service.demoCell, err = geoidx.CellIDFromLatLng(37.7749, -122.4194)
	if err != nil {
		return fail(err)
	}
	return service, nil
}

func (service *demoService) Close() error {
	return errors.Join(service.store.Close(), os.RemoveAll(service.dir))
}

func (service *demoService) searchOnce(ctx context.Context, request searchRequest) ([]common.ImageID, error) {
	favorites, err := service.engine.Values(common.NamespaceTag, bitmapidx.EnumStringValue("favorite"))
	if err != nil {
		return nil, err
	}
	during, err := service.engine.During(100, request.Start, request.End,
		search.ExactTimestamp(func(id common.ImageID) (uint64, bool) {
			value, ok := service.canonical[id]
			return value, ok
		}))
	if err != nil {
		return nil, err
	}
	cells, err := service.engine.InCells(common.NamespaceLocation, request.CellIDs...)
	if err != nil {
		return nil, err
	}
	plan, err := service.engine.Compile(common.EntityImage, favorites, during, cells)
	if err != nil {
		return nil, err
	}
	return service.executor.AppendIDs(ctx, nil, plan)
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
