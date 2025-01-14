// Copyright 2022 Linkall Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package vsb

import (
	// standard libraries.
	"context"
	stderr "errors"
	"sync/atomic"
	"time"

	// first-party libraries.
	"github.com/linkall-labs/vanus/observability/log"
	"go.opentelemetry.io/otel/attribute"

	// this project.
	"github.com/linkall-labs/vanus/internal/store/block"
	ceschema "github.com/linkall-labs/vanus/internal/store/schema/ce"
	"github.com/linkall-labs/vanus/internal/store/vsb/index"
	"github.com/linkall-labs/vanus/pkg/errors"
)

var errCorruptedFragment = stderr.New("vsb: corrupted fragment")

type appendContext struct {
	seq      int64
	offset   int64
	archived uint32
}

// Make sure appendContext implements block.AppendContext.
var _ block.AppendContext = (*appendContext)(nil)

func (c *appendContext) size(dataOffset int64) int64 {
	return c.offset - dataOffset
}

func (c *appendContext) WriteOffset() int64 {
	return c.offset
}

func (c *appendContext) Archived() bool {
	return c.archived != 0
}

// Make sure vsBlock implements block.TwoPCAppender.
var _ block.TwoPCAppender = (*vsBlock)(nil)

func (b *vsBlock) NewAppendContext(last block.Fragment) block.AppendContext {
	if last != nil {
		_, entry, _ := b.dec.UnmarshalLast(last.Payload())
		seq := ceschema.SequenceNumber(entry)
		actx := &appendContext{
			seq:    seq + 1,
			offset: last.EndOffset(),
		}
		if ceschema.EntryType(entry) == ceschema.End {
			actx.archived = 1
		}
		return actx
	}

	// Copy append context.
	actx := b.actx
	return &actx
}

func (b *vsBlock) PrepareAppend(
	ctx context.Context, appendCtx block.AppendContext, entries ...block.Entry,
) ([]int64, block.Fragment, bool, error) {
	_, span := b.tracer.Start(ctx, "PrepareAppend")
	defer span.End()

	actx, _ := appendCtx.(*appendContext)

	num := int64(len(entries))
	ents := make([]block.Entry, num)
	seqs := make([]int64, num)

	// TODO(james.yin): fill auto fields in a general way.
	now := time.Now().UnixMilli()
	for i := int64(0); i < num; i++ {
		seq := actx.seq + i
		ents[i] = wrapEntry(entries[i], ceschema.CloudEvent, seq, now)
		seqs[i] = seq
	}

	frag := newFragment(actx.offset, ents, b.enc)

	actx.offset += int64(frag.Size())
	actx.seq += num

	return seqs, frag, actx.size(b.dataOffset) >= b.capacity, nil
}

func (b *vsBlock) PrepareArchive(ctx context.Context, appendCtx block.AppendContext) (block.Fragment, error) {
	_, span := b.tracer.Start(ctx, "PrepareArchive")
	defer span.End()

	actx, _ := appendCtx.(*appendContext)

	end := wrapEntry(&block.EmptyEntryExt{}, ceschema.End, actx.seq, time.Now().UnixMilli())
	frag := newFragment(actx.offset, []block.Entry{end}, b.enc)

	actx.offset += int64(frag.Size())
	actx.seq++
	actx.archived = 1

	return frag, nil
}

func (b *vsBlock) CommitAppend(ctx context.Context, frags ...block.Fragment) (bool, error) {
	ctx, span := b.tracer.Start(ctx, "CommitAppend")
	defer span.End()

	frags, err := b.trimFragments(ctx, frags)
	if err != nil {
		return false, err
	}

	if len(frags) == 0 {
		return false, nil
	}

	if err = b.checkFragments(ctx, frags); err != nil {
		return false, err
	}

	var sz int
	for _, frag := range frags {
		sz += frag.Size()
	}
	data := make([]byte, sz)
	base := frags[0].StartOffset()
	for _, frag := range frags {
		copy(data[frag.StartOffset()-base:], frag.Payload())
	}

	indexes, entryCount, archived, err := b.buildIndexes(ctx, base, data)
	if err != nil {
		return false, err
	}
	if !archived && len(indexes) == 0 {
		return false, nil
	}

	_, wSpan := b.tracer.Start(ctx, "writeFile")
	entrySize, err := b.f.WriteAt(data[b.actx.offset-base:], b.actx.offset)
	if err != nil {
		wSpan.End()
		return false, err
	}
	wSpan.End()

	span.AddEvent("Acquiring lock")
	b.mu.Lock()
	span.AddEvent("Got lock")

	span.SetAttributes(
		attribute.Int("index_count", len(b.indexes)),
		attribute.Int("index_capacity", cap(b.indexes)),
	)

	b.indexes = append(b.indexes, indexes...)
	b.actx.seq += entryCount
	b.actx.offset += int64(entrySize)
	if archived {
		atomic.StoreUint32(&b.actx.archived, 1)
	}

	span.AddEvent("Release lock")
	b.mu.Unlock()

	if archived {
		m, i := makeSnapshot(b.actx, b.indexes)

		b.wg.Add(1)
		go func() {
			defer b.wg.Done()
			if n, err := b.appendIndexEntry(ctx, i, m.writeOffset); err == nil {
				b.indexOffset = m.writeOffset
				b.indexLength = n
			}
			_ = b.persistHeader(ctx, m)
		}()

		if b.lis != nil {
			b.lis.OnArchived(b.stat(m, i))
		}
	}

	span.SetAttributes(
		attribute.Int("entry_count", int(entryCount)),
		attribute.Int("entry_size", entrySize),
		attribute.Bool("archived", archived))

	return archived, nil
}

func (b *vsBlock) buildIndexes(ctx context.Context, base int64, data []byte) ([]index.Index, int64, bool, error) {
	_, span := b.tracer.Start(ctx, "buildIndexes")
	defer span.End()

	var archived bool
	indexes := make([]index.Index, 0, 1)
	expected := b.actx.seq
	for off, sz := 0, len(data); off < sz; {
		n, entry, _ := b.dec.Unmarshal(data[off:])
		switch seq := ceschema.SequenceNumber(entry); {
		case seq == expected:
			expected++
		case seq < expected && len(indexes) == 0:
			continue
		default:
			return nil, 0, false, errCorruptedFragment
		}

		if ceschema.EntryType(entry) == ceschema.End {
			// End entry must be the last.
			if off+n != sz {
				return nil, 0, false, errCorruptedFragment
			}
			archived = true
			break
		}

		idx := index.NewIndex(base+int64(off), int32(n), index.WithEntry(entry))
		indexes = append(indexes, idx)

		off += n
	}

	return indexes, expected - b.actx.seq, archived, nil
}

func (b *vsBlock) appendIndexEntry(ctx context.Context, indexes []index.Index, off int64) (int, error) {
	entry := index.NewEntry(indexes)
	sz := b.enc.Size(entry)
	data := make([]byte, sz)
	if _, err := b.enc.MarshalTo(ctx, entry, data); err != nil {
		return 0, err
	}

	if _, err := b.f.WriteAt(data, off); err != nil {
		return 0, err
	}

	return sz, nil
}

func (b *vsBlock) trimFragments(ctx context.Context, frags []block.Fragment) ([]block.Fragment, error) {
	off := b.actx.offset
	for i := 0; i < len(frags); i++ {
		switch frag := frags[i]; {
		case frag.EndOffset() <= off:
			log.Info(ctx, "vsb: data of fragment has been written, skip this entry.", map[string]interface{}{
				"block_id":              b.id,
				"expected":              off,
				"fragment_start_offset": frag.StartOffset(),
				"fragment_end_offset":   frag.EndOffset(),
			})
			continue
		case frag.StartOffset() > off:
			log.Error(ctx, "vsb: missing some fragments.", map[string]interface{}{
				"block_id": b.id,
				"expected": off,
				"found":    frag.StartOffset(),
			})
			return nil, errors.ErrInternal
		}
		if i != 0 {
			return frags[i:], nil
		}
		return frags, nil
	}
	return nil, nil
}

func (b *vsBlock) checkFragments(ctx context.Context, frags []block.Fragment) error {
	// if firstSo := frags[0].StartOffset(); firstSo > int64(b.actx.offset) {
	// 	log.Error(ctx, "vsb: missing some fragments.", map[string]interface{}{
	// 		"block_id": b.id,
	// 		"expected": b.actx.offset,
	// 		"found":    firstSo,
	// 	})
	// 	return errors.ErrInternal
	// }

	for i := 1; i < len(frags); i++ {
		prevEo := frags[i-1].EndOffset()
		nextSo := frags[i].StartOffset()
		if prevEo != nextSo {
			log.Error(ctx, "vsb: fragments is discontinuous.", map[string]interface{}{
				"block_id":            b.id,
				"next_start_offset":   nextSo,
				"previous_end_offset": prevEo,
			})
			return errors.ErrInternal
		}
	}

	return nil
}
