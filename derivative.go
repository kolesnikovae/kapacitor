package kapacitor

import (
	"log"
	"sync"
	"time"

	"github.com/influxdata/kapacitor/expvar"
	"github.com/influxdata/kapacitor/models"
	"github.com/influxdata/kapacitor/pipeline"
)

type DerivativeNode struct {
	node
	d *pipeline.DerivativeNode
}

// Create a new derivative node.
func newDerivativeNode(et *ExecutingTask, n *pipeline.DerivativeNode, l *log.Logger) (*DerivativeNode, error) {
	dn := &DerivativeNode{
		node: node{Node: n, et: et, logger: l},
		d:    n,
	}
	// Create stateful expressions
	dn.node.runF = dn.runDerivative
	return dn, nil
}

func (d *DerivativeNode) runDerivative([]byte) error {
	switch d.Provides() {
	case pipeline.StreamEdge:
		var mu sync.RWMutex
		previous := make(map[models.GroupID]models.Point)
		cardinalityGauge := expvar.NewIntFuncGauge(func() int64 {
			mu.RLock()
			l := len(previous)
			mu.RUnlock()
			return int64(l)
		})
		d.statMap.Set(statsCardinalityGauge, cardinalityGauge)
		for p, ok := d.ins[0].NextPoint(); ok; p, ok = d.ins[0].NextPoint() {
			d.timer.Start()
			mu.RLock()
			pr, ok := previous[p.Group]
			mu.RUnlock()
			if !ok {
				mu.Lock()
				previous[p.Group] = p
				mu.Unlock()
				d.timer.Stop()
				continue
			}

			value, ok := d.derivative(pr.Fields, p.Fields, pr.Time, p.Time)
			if ok {
				fields := pr.Fields.Copy()
				fields[d.d.As] = value
				pr.Fields = fields
				d.timer.Pause()
				for _, child := range d.outs {
					err := child.CollectPoint(pr)
					if err != nil {
						return err
					}
				}
				d.timer.Resume()
			}
			mu.Lock()
			previous[p.Group] = p
			mu.Unlock()
			d.timer.Stop()
		}
	case pipeline.BatchEdge:
		for b, ok := d.ins[0].NextBatch(); ok; b, ok = d.ins[0].NextBatch() {
			d.timer.Start()
			if len(b.Points) > 0 {
				pr := b.Points[0]
				var p models.BatchPoint
				for i := 1; i < len(b.Points); i++ {
					p = b.Points[i]
					value, ok := d.derivative(pr.Fields, p.Fields, pr.Time, p.Time)
					if ok {
						fields := pr.Fields.Copy()
						fields[d.d.As] = value
						b.Points[i-1].Fields = fields
					} else {
						b.Points = append(b.Points[:i-1], b.Points[i:]...)
						i--
					}
					pr = p
				}
				b.Points = b.Points[:len(b.Points)-1]
			}
			d.timer.Stop()
			for _, child := range d.outs {
				err := child.CollectBatch(b)
				if err != nil {
					return err
				}
			}
		}
	}
	return nil
}

func (d *DerivativeNode) derivative(prev, curr models.Fields, prevTime, currTime time.Time) (float64, bool) {
	f0, ok := numToFloat(prev[d.d.Field])
	if !ok {
		d.incrementErrorCount()
		d.logger.Printf("E! cannot apply derivative to type %T", prev[d.d.Field])
		return 0, false
	}

	f1, ok := numToFloat(curr[d.d.Field])
	if !ok {
		d.incrementErrorCount()
		d.logger.Printf("E! cannot apply derivative to type %T", curr[d.d.Field])
		return 0, false
	}

	elapsed := float64(currTime.Sub(prevTime))
	if elapsed == 0 {
		d.incrementErrorCount()
		d.logger.Printf("E! cannot perform derivative elapsed time was 0")
		return 0, false
	}
	diff := f1 - f0
	// Drop negative values for non-negative derivatives
	if d.d.NonNegativeFlag && diff < 0 {
		return 0, false
	}

	value := float64(diff) / (elapsed / float64(d.d.Unit))
	return value, true
}

func numToFloat(num interface{}) (float64, bool) {
	switch n := num.(type) {
	case int64:
		return float64(n), true
	case float64:
		return n, true
	default:
		return 0, false
	}
}
