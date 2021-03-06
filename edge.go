package kapacitor

import (
	"errors"
	"fmt"
	"log"
	"sync"

	"github.com/influxdata/kapacitor/expvar"
	"github.com/influxdata/kapacitor/models"
	"github.com/influxdata/kapacitor/pipeline"
)

const (
	statCollected = "collected"
	statEmitted   = "emitted"

	defaultEdgeBufferSize = 1000
)

var ErrAborted = errors.New("edged aborted")

type StreamCollector interface {
	CollectPoint(models.Point) error
	Close()
}

type BatchCollector interface {
	CollectBatch(models.Batch) error
	Close()
}

type Edge struct {
	stream chan models.Point
	batch  chan models.Batch

	logger     *log.Logger
	aborted    chan struct{}
	statsKey   string
	collected  *expvar.Int
	emitted    *expvar.Int
	statMap    *expvar.Map
	groupMu    sync.RWMutex
	groupStats map[models.GroupID]*edgeStat
}

func newEdge(taskName, parentName, childName string, t pipeline.EdgeType, size int, logService LogService) *Edge {
	tags := map[string]string{
		"task":   taskName,
		"parent": parentName,
		"child":  childName,
		"type":   t.String(),
	}
	key, sm := NewStatistics("edges", tags)
	collected := &expvar.Int{}
	emitted := &expvar.Int{}
	sm.Set(statCollected, collected)
	sm.Set(statEmitted, emitted)
	e := &Edge{
		statsKey:   key,
		statMap:    sm,
		collected:  collected,
		emitted:    emitted,
		aborted:    make(chan struct{}),
		groupStats: make(map[models.GroupID]*edgeStat),
	}
	name := fmt.Sprintf("%s|%s->%s", taskName, parentName, childName)
	e.logger = logService.NewLogger(fmt.Sprintf("[edge:%s] ", name), log.LstdFlags)
	switch t {
	case pipeline.StreamEdge:
		e.stream = make(chan models.Point, size)
	case pipeline.BatchEdge:
		e.batch = make(chan models.Batch, size)
	}
	return e
}

func (e *Edge) emittedCount() int64 {
	return e.emitted.IntValue()
}

func (e *Edge) collectedCount() int64 {
	return e.collected.IntValue()
}

// Stats for a given group for this edge
type edgeStat struct {
	collected int64
	emitted   int64
	tags      models.Tags
	dims      []string
}

// Get a snapshot of the current group statistics for this edge
func (e *Edge) readGroupStats(f func(group models.GroupID, collected, emitted int64, tags models.Tags, dims []string)) {
	e.groupMu.RLock()
	defer e.groupMu.RUnlock()
	for group, stats := range e.groupStats {
		f(
			group,
			stats.collected,
			stats.emitted,
			stats.tags,
			stats.dims,
		)
	}
}

// Close the edge, this can only be called after all
// collect calls to the edge have finished.
func (e *Edge) Close() {
	e.logger.Printf(
		"D! closing c: %d e: %d\n",
		e.collected.IntValue(),
		e.emitted.IntValue(),
	)
	if e.stream != nil {
		close(e.stream)
	}
	if e.batch != nil {
		close(e.batch)
	}
	DeleteStatistics(e.statsKey)
}

// Abort all next and collect calls.
// Items in flight may or may not be processed.
func (e *Edge) Abort() {
	close(e.aborted)
	e.logger.Printf(
		"I! aborting c: %d e: %d\n",
		e.collected.IntValue(),
		e.emitted.IntValue(),
	)
}

func (e *Edge) Next() (p models.PointInterface, ok bool) {
	if e.stream != nil {
		return e.NextPoint()
	}
	return e.NextBatch()
}

func (e *Edge) NextPoint() (p models.Point, ok bool) {
	select {
	case <-e.aborted:
	case p, ok = <-e.stream:
		if ok {
			e.emitted.Add(1)
			e.incEmitted(p.Group, p.Tags, p.Dimensions)
		}
	}
	return
}

func (e *Edge) NextBatch() (b models.Batch, ok bool) {
	select {
	case <-e.aborted:
	case b, ok = <-e.batch:
		if ok {
			e.emitted.Add(1)
			e.incEmitted(b.Group, b.Tags, b.PointDimensions())
		}
	}
	return
}

func (e *Edge) CollectPoint(p models.Point) error {
	e.collected.Add(1)
	e.incCollected(p.Group, p.Tags, p.Dimensions)
	select {
	case <-e.aborted:
		return ErrAborted
	case e.stream <- p:
		return nil
	}
}

func (e *Edge) CollectBatch(b models.Batch) error {
	e.collected.Add(1)
	e.incCollected(b.Group, b.Tags, b.PointDimensions())
	select {
	case <-e.aborted:
		return ErrAborted
	case e.batch <- b:
		return nil
	}
}

// Increment the emitted count of the group for this edge.
func (e *Edge) incEmitted(group models.GroupID, tags models.Tags, dims []string) {
	e.groupMu.Lock()
	defer e.groupMu.Unlock()
	if stats, ok := e.groupStats[group]; ok {
		stats.emitted += 1
	} else {
		stats = &edgeStat{
			emitted: 1,
			tags:    tags,
			dims:    dims,
		}
		e.groupStats[group] = stats
	}
}

// Increment the collected count of the group for this edge.
func (e *Edge) incCollected(group models.GroupID, tags models.Tags, dims []string) {
	e.groupMu.Lock()
	defer e.groupMu.Unlock()
	if stats, ok := e.groupStats[group]; ok {
		stats.collected += 1
	} else {
		stats = &edgeStat{
			collected: 1,
			tags:      tags,
			dims:      dims,
		}
		e.groupStats[group] = stats
	}
}
