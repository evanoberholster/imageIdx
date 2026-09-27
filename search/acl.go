package search

import (
	"errors"
	"slices"

	common "github.com/evanoberholster/imageIdx/common"
	"github.com/evanoberholster/imageIdx/index/bitmapidx"
	"github.com/evanoberholster/imageIdx/roar"
)

var errACLStore = errors.New("search: WithACL requires a store with ACL namespaces")

func compileACL(store *bitmapidx.Store, clause aclClause) (stage, error) {
	if store == nil {
		return stage{}, errACLStore
	}
	user, err := NewUserACL(common.UserID(clause.userID))
	if err != nil {
		return stage{}, err
	}
	users, ok := store.Lookup(common.NamespaceUserACL)
	if !ok || users.Spec().Kind != common.KindBitmapEnum {
		return stage{}, errACLStore
	}
	userQuery, err := users.Compile(user.Value())
	if err != nil {
		return stage{}, err
	}
	queries := make([]bitmapidx.Query, 0, 1+len(clause.groupIDs))
	queries = append(queries, userQuery)
	if len(clause.groupIDs) != 0 {
		groups, ok := store.Lookup(common.NamespaceGroupACL)
		if !ok || groups.Spec().Kind != common.KindBitmapEnum {
			return stage{}, errACLStore
		}
		seen := make([]uint64, 0, len(clause.groupIDs))
		for _, id := range clause.groupIDs {
			group, err := NewGroupACL(common.GroupID(id))
			if err != nil {
				return stage{}, err
			}
			if slices.Contains(seen, id) {
				continue
			}
			seen = append(seen, id)
			query, err := groups.Compile(group.Value())
			if err != nil {
				return stage{}, err
			}
			queries = append(queries, query)
		}
	}
	return stage{kind: StageACL, aclQueries: queries, estimate: aclEstimate(queries)}, nil
}

// aclEstimate is an upper bound for the union. It is a planning hint only.
func aclEstimate(queries []bitmapidx.Query) uint64 {
	var total uint64
	for _, query := range queries {
		estimate := query.Estimate()
		if estimate > ^uint64(0)-total {
			return ^uint64(0)
		}
		total += estimate
	}
	return total
}

func (s *stage) aclBitmap() (roar.Bitmap, error) {
	if len(s.aclQueries) == 1 {
		return s.aclQueries[0].Match()
	}
	var inline [8]roar.Bitmap
	bitmaps := inline[:0]
	if len(s.aclQueries) > len(inline) {
		bitmaps = make([]roar.Bitmap, 0, len(s.aclQueries))
	}
	for _, query := range s.aclQueries {
		bitmap, err := query.Match()
		if err != nil {
			return roar.Bitmap{}, err
		}
		bitmaps = append(bitmaps, bitmap)
	}
	return roar.OrMany(bitmaps...), nil
}

// filterACL keeps input order and duplicates while applying OR across the
// principal postings. Reusable workspace scratch avoids materializing large
// ACL posting unions for small candidate batches.
func (s *stage) filterACL(dst, candidates []ImageID, ws *Workspace) ([]ImageID, error) {
	if len(candidates) == 0 || len(s.aclQueries) == 0 {
		return dst, nil
	}
	if len(s.aclQueries) == 1 {
		return s.aclQueries[0].Filter(dst, candidates)
	}
	if ws == nil {
		ws = &Workspace{}
	}
	if cap(ws.aclMask) < len(candidates) {
		ws.aclMask = make([]bool, len(candidates))
	} else {
		ws.aclMask = ws.aclMask[:len(candidates)]
		clear(ws.aclMask)
	}
	selected := 0
	for _, query := range s.aclQueries {
		var err error
		ws.aclMatches, err = query.Filter(ws.aclMatches[:0], candidates)
		if err != nil {
			return dst, err
		}
		matched := ws.aclMatches
		for n, read := 0, 0; n < len(candidates) && read < len(matched); n++ {
			if candidates[n] != matched[read] {
				continue
			}
			if !ws.aclMask[n] {
				ws.aclMask[n] = true
				selected++
			}
			read++
		}
		if selected == len(candidates) {
			break
		}
	}
	for n, id := range candidates {
		if ws.aclMask[n] {
			dst = append(dst, id)
		}
	}
	return dst, nil
}
