// SPDX-License-Identifier: Apache-2.0
// Copyright Authors of Cilium

package statedb

import (
	"context"

	"github.com/cilium/cilium/pkg/hive"
	"github.com/cilium/cilium/pkg/hive/cell"
	"github.com/cilium/cilium/pkg/hive/job"
)

type DeriveResult int

const (
	DeriveInsert DeriveResult = 0 // Insert the object
	DeriveUpdate DeriveResult = 1 // Update the object (if it exists)
	DeriveDelete DeriveResult = 2 // Delete the object
	DeriveSkip   DeriveResult = 3 // Skip
)

type DeriveParams[In, Out any] struct {
	cell.In

	Lifecycle hive.Lifecycle
	Jobs      job.Registry
	Scope     cell.Scope
	DB        *DB
	InTable   Table[In]
	OutTable  RWTable[Out]
}

// Derive constructs and registers a job to transform objects from the input table to the
// output table, e.g. derive the output table from the input table. Useful when constructing
// a reconciler that has its desired state solely derived from a single table. For example
// the bandwidth manager's desired state is directly derived from the devices table.
//
// Derive is parametrized with the transform function that transforms the input object
// into the output object. If the transform function returns false, then the object
// is skipped.
//
// Example use:
//
//	cell.Invoke(
//	  statedb.Derive[*tables.Device, *Foo](
//	    func(d *Device, deleted bool) (*Foo, DeriveResult) {
//	      if deleted {
//	        return &Foo{Index: d.Index}, DeriveDelete
//	      }
//	      return &Foo{Index: d.Index}, DeriveInsert
//	    }),
//	)
func Derive[In, Out any](jobName string, transform func(obj In, deleted bool) (Out, DeriveResult)) func(DeriveParams[In, Out]) {
	return func(p DeriveParams[In, Out]) {
		g := p.Jobs.NewGroup(p.Scope)
		g.Add(job.OneShot(
			jobName,
			derive[In, Out]{p, jobName, transform}.loop),
		)
		p.Lifecycle.Append(g)
	}

}

type derive[In, Out any] struct {
	DeriveParams[In, Out]
	jobName   string
	transform func(obj In, deleted bool) (Out, DeriveResult)
}

func (d derive[In, Out]) loop(ctx context.Context, health cell.HealthReporter) error {
	out := d.OutTable
	wtxn := d.DB.WriteTxn(d.InTable)
	tracker, err := d.InTable.DeleteTracker(wtxn, d.jobName)
	if err != nil {
		wtxn.Abort()
		return err
	}
	wtxn.Commit()
	defer tracker.Close()
	revision := Revision(0)
	for {
		wtxn := d.DB.WriteTxn(out)

		var watch <-chan struct{}
		revision, watch, err = tracker.Process(
			wtxn,
			revision,
			func(obj In, deleted bool, rev Revision) (err error) {
				outObj, result := d.transform(obj, deleted)
				switch result {
				case DeriveInsert:
					_, _, err = out.Insert(wtxn, outObj)
				case DeriveUpdate:
					_, _, found := out.First(wtxn, out.PrimaryIndexer().QueryFromObject(outObj))
					if found {
						_, _, err = out.Insert(wtxn, outObj)
					}
				case DeriveDelete:
					_, _, err = out.Delete(wtxn, outObj)
				case DeriveSkip:
				}
				return err
			},
		)
		wtxn.Commit()

		if err != nil {
			return err
		}

		select {
		case <-watch:
		case <-ctx.Done():
			return nil
		}
	}
}
