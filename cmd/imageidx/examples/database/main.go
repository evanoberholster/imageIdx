// Command database demonstrates the stable V1 database workflow: schema
// bootstrap, durable commit, indexing wait, search, canonical read,
// delete/reinsert, checkpoint, and close/reopen.
package main

import (
	"context"
	"encoding/binary"
	"fmt"
	"log"
	"os"
	"time"

	"github.com/evanoberholster/imageIdx/database"
	"github.com/evanoberholster/imageIdx/index"
	"github.com/evanoberholster/imageIdx/index/bitmapidx"
	"github.com/evanoberholster/imageIdx/schema"
	"github.com/evanoberholster/imageIdx/search"
)

const tagNamespace schema.Namespace = 101

func decodeProjection(p []byte) (index.Entry, error) {
	if len(p) != 9 {
		return index.Entry{}, database.ErrCorruptJournal
	}
	return index.Entry{
		ValueID: binary.BigEndian.Uint64(p[:8]),
		Bitmaps: []index.BitmapValue{{
			Namespace: tagNamespace,
			Value:     bitmapidx.EnumBytesValue(p[8:9]),
		}},
	}, nil
}

func encodeProjection(id schema.ImageID, tag byte) []byte {
	p := make([]byte, 9)
	binary.BigEndian.PutUint64(p[:8], uint64(id))
	p[8] = tag
	return p
}

func query(ctx context.Context, db *database.DB, tag byte) ([]schema.ImageID, error) {
	engine, err := search.New(db.Index())
	if err != nil {
		return nil, err
	}
	clause, err := engine.Values(tagNamespace, bitmapidx.EnumBytesValue([]byte{tag}))
	if err != nil {
		return nil, err
	}
	plan, err := engine.Compile(schema.EntityImage, clause)
	if err != nil {
		return nil, err
	}
	return plan.AppendIDs(ctx, nil, search.NewWorkspace(128))
}

func main() {
	root, err := os.MkdirTemp("", "imageidx-database-")
	if err != nil {
		log.Fatal(err)
	}
	defer os.RemoveAll(root)
	dataDir, indexDir := root+"/data", root+"/index"

	defs := schema.NewDefinitions()
	tag, err := index.BitmapDef(bitmapidx.NamespaceSpec{
		Namespace: tagNamespace,
		Kind:      schema.KindBitmapEnum,
	}, "tags", schema.EntityImage)
	if err != nil {
		log.Fatal(err)
	}
	if err := defs.Register(tag); err != nil {
		log.Fatal(err)
	}

	// Bootstrap the derived index once. Later opens use database.Open directly.
	idx, err := index.Create(indexDir, index.Spec{Schema: defs})
	if err != nil {
		log.Fatal(err)
	}
	if err := idx.Close(); err != nil {
		log.Fatal(err)
	}

	db, err := database.Open(dataDir, indexDir, decodeProjection, database.Options{BatchSize: 64})
	if err != nil {
		log.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	id := schema.ImageID(100)
	generation, err := db.Commit(database.Mutation{
		Entity:     schema.EntityImage,
		ValueID:    id,
		Record:     []byte("first canonical record"),
		Projection: encodeProjection(id, 'f'),
	})
	if err != nil {
		log.Fatal(err)
	}
	if err := db.WaitIndexed(ctx, generation); err != nil {
		log.Fatal(err)
	}

	ids, err := query(ctx, db, 'f')
	if err != nil {
		log.Fatal(err)
	}
	record, err := db.Get(schema.EntityImage, id)
	if err != nil {
		log.Fatal(err)
	}
	fmt.Printf("after insert: ids=%v record=%q\n", ids, record)

	generation, err = db.Commit(database.Mutation{
		Entity: schema.EntityImage, ValueID: id, Delete: true,
	})
	if err != nil {
		log.Fatal(err)
	}
	if err := db.WaitIndexed(ctx, generation); err != nil {
		log.Fatal(err)
	}
	ids, err = query(ctx, db, 'f')
	if err != nil {
		log.Fatal(err)
	}
	fmt.Printf("after delete: ids=%v\n", ids)

	generation, err = db.Commit(database.Mutation{
		Entity:     schema.EntityImage,
		ValueID:    id,
		Record:     []byte("reinserted canonical record"),
		Projection: encodeProjection(id, 'f'),
	})
	if err != nil {
		log.Fatal(err)
	}
	if err := db.WaitIndexed(ctx, generation); err != nil {
		log.Fatal(err)
	}
	ids, err = query(ctx, db, 'f')
	if err != nil {
		log.Fatal(err)
	}
	fmt.Printf("after reinsert: ids=%v\n", ids)
	if err := db.Checkpoint(); err != nil {
		log.Fatal(err)
	}
	if err := db.Close(); err != nil {
		log.Fatal(err)
	}

	// Reopen the same V1 database. No index creation or schema resupply is needed.
	db, err = database.Open(dataDir, indexDir, decodeProjection, database.Options{})
	if err != nil {
		log.Fatal(err)
	}
	defer db.Close()

	ids, err = query(ctx, db, 'f')
	if err != nil {
		log.Fatal(err)
	}
	record, err = db.Get(schema.EntityImage, id)
	if err != nil {
		log.Fatal(err)
	}
	fmt.Printf("after reopen: ids=%v record=%q\n", ids, record)
}
