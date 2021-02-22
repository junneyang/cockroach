// Copyright 2020 The Cockroach Authors.
//
// Licensed as a CockroachDB Enterprise file under the Cockroach Community
// License (the "License"); you may not use this file except in compliance with
// the License. You may obtain a copy of the License at
//
//     https://github.com/cockroachdb/cockroach/blob/master/licenses/CCL.txt

package backupccl

import (
	"context"
	"fmt"
	"time"

	"github.com/cockroachdb/cockroach/pkg/ccl/storageccl"
	"github.com/cockroachdb/cockroach/pkg/keys"
	"github.com/cockroachdb/cockroach/pkg/kv"
	"github.com/cockroachdb/cockroach/pkg/roachpb"
	"github.com/cockroachdb/cockroach/pkg/sql/catalog/descpb"
	"github.com/cockroachdb/cockroach/pkg/sql/execinfra"
	"github.com/cockroachdb/cockroach/pkg/sql/execinfrapb"
	"github.com/cockroachdb/cockroach/pkg/sql/rowenc"
	"github.com/cockroachdb/cockroach/pkg/sql/rowexec"
	"github.com/cockroachdb/cockroach/pkg/sql/sem/tree"
	"github.com/cockroachdb/cockroach/pkg/sql/types"
	"github.com/cockroachdb/cockroach/pkg/util/ctxgroup"
	"github.com/cockroachdb/cockroach/pkg/util/log"
	"github.com/cockroachdb/cockroach/pkg/util/protoutil"
	"github.com/cockroachdb/errors"
)

type splitAndScatterer interface {
	// splitAndScatterSpan issues a split request at a given key and then scatters
	// the range around the cluster. It returns the node ID of the leaseholder of
	// the span after the scatter.
	splitAndScatterKey(ctx context.Context, codec keys.SQLCodec, db *kv.DB, kr *storageccl.KeyRewriter, key roachpb.Key, randomizeLeases bool) (roachpb.NodeID, error)
}

type noopSplitAndScatterer struct{}

// splitAndScatterKey implements the splitAndScatterer interface.
// It is safe to always return 0 since during processor planning the range
// router has a `DefaultStream` specified in case the range generated by this
// node ID doesn't match any of the result router's spans.
func (n noopSplitAndScatterer) splitAndScatterKey(
	_ context.Context, _ keys.SQLCodec, _ *kv.DB, _ *storageccl.KeyRewriter, _ roachpb.Key, _ bool,
) (roachpb.NodeID, error) {
	return 0, nil
}

// dbSplitAndScatter is the production implementation of this processor's
// scatterer. It actually issues the split and scatter requests for KV. This is
// mocked out in some tests.
type dbSplitAndScatterer struct{}

// splitAndScatterKey implements the splitAndScatterer interface.
// It splits and scatters a span specified by a given key, and returns the node
// to which the span was scattered. If the destination node could not be
// determined, node ID of 0 is returned.
func (s dbSplitAndScatterer) splitAndScatterKey(
	ctx context.Context,
	codec keys.SQLCodec,
	db *kv.DB,
	kr *storageccl.KeyRewriter,
	key roachpb.Key,
	randomizeLeases bool,
) (roachpb.NodeID, error) {
	expirationTime := db.Clock().Now().Add(time.Hour.Nanoseconds(), 0)
	newSpanKey, err := rewriteBackupSpanKey(codec, kr, key)
	if err != nil {
		return 0, err
	}

	// TODO(pbardea): Really, this should be splitting the Key of the _next_
	// entry.
	log.VEventf(ctx, 1, "presplitting new key %+v", newSpanKey)
	if err := db.AdminSplit(ctx, newSpanKey, expirationTime); err != nil {
		return 0, errors.Wrapf(err, "splitting key %s", newSpanKey)
	}

	log.VEventf(ctx, 1, "scattering new key %+v", newSpanKey)
	req := &roachpb.AdminScatterRequest{
		RequestHeader: roachpb.RequestHeaderFromSpan(roachpb.Span{
			Key:    newSpanKey,
			EndKey: newSpanKey.Next(),
		}),
		// This is a bit of a hack, but it seems to be an effective one (see #36665
		// for graphs). As of the commit that added this, scatter is not very good
		// at actually balancing leases. This is likely for two reasons: 1) there's
		// almost certainly some regression in scatter's behavior, it used to work
		// much better and 2) scatter has to operate by balancing leases for all
		// ranges in a cluster, but in RESTORE, we really just want it to be
		// balancing the span being restored into.
		RandomizeLeases: randomizeLeases,
	}

	res, pErr := kv.SendWrapped(ctx, db.NonTransactionalSender(), req)
	if pErr != nil {
		// TODO(pbardea): Unfortunately, Scatter is still too unreliable to
		// fail the RESTORE when Scatter fails. I'm uncomfortable that
		// this could break entirely and not start failing the tests,
		// but on the bright side, it doesn't affect correctness, only
		// throughput.
		log.Errorf(ctx, "failed to scatter span [%s,%s): %+v",
			newSpanKey, newSpanKey.Next(), pErr.GoError())
		return 0, nil
	}

	return s.findDestination(res.(*roachpb.AdminScatterResponse)), nil
}

// findDestination returns the node ID of the node of the destination of the
// AdminScatter request. If the destination cannot be found, 0 is returned.
func (s dbSplitAndScatterer) findDestination(res *roachpb.AdminScatterResponse) roachpb.NodeID {
	// A request from a 20.1 node will not have a RangeInfos with a lease.
	// For this mixed-version state, we'll report the destination as node 0
	// and suffer a bit of inefficiency.
	if len(res.RangeInfos) > 0 {
		// If the lease is not populated, we return the 0 value anyway. We receive 1
		// RangeInfo per range that was scattered. Since we send a scatter request
		// to each range that we make, we are only interested in the first range,
		// which contains the key at which we're splitting and scattering.
		return res.RangeInfos[0].Lease.Replica.NodeID
	}

	return roachpb.NodeID(0)
}

const splitAndScatterProcessorName = "splitAndScatter"

var splitAndScatterOutputTypes = []*types.T{
	types.Bytes, // Span key for the range router
	types.Bytes, // RestoreDataEntry bytes
}

// splitAndScatterProcessor is given a set of spans (specified as
// RestoreSpanEntry's) to distribute across the cluster. Depending on which node
// the span ends up on, it forwards RestoreSpanEntry as bytes along with the key
// of the span on a row. It expects an output RangeRouter and before it emits
// each row, it updates the entry in the RangeRouter's map with the destination
// of the scatter.
type splitAndScatterProcessor struct {
	execinfra.ProcessorBase

	flowCtx *execinfra.FlowCtx
	spec    execinfrapb.SplitAndScatterSpec
	output  execinfra.RowReceiver

	scatterer      splitAndScatterer
	stopScattering context.CancelFunc

	doneScatterCh chan entryNode
	// A cache for routing datums, so only 1 is allocated per node.
	routingDatumCache map[roachpb.NodeID]rowenc.EncDatum
	scatterErr        error
}

var _ execinfra.Processor = &splitAndScatterProcessor{}

// OutputTypes implements the execinfra.Processor interface.
func (ssp *splitAndScatterProcessor) OutputTypes() []*types.T {
	return splitAndScatterOutputTypes
}

func newSplitAndScatterProcessor(
	flowCtx *execinfra.FlowCtx,
	processorID int32,
	spec execinfrapb.SplitAndScatterSpec,
	post *execinfrapb.PostProcessSpec,
	output execinfra.RowReceiver,
) (execinfra.Processor, error) {
	numEntries := 0
	for _, chunk := range spec.Chunks {
		numEntries += len(chunk.Entries)
	}

	var scatterer splitAndScatterer = dbSplitAndScatterer{}
	if !flowCtx.Cfg.Codec.ForSystemTenant() {
		scatterer = noopSplitAndScatterer{}
	}
	ssp := &splitAndScatterProcessor{
		flowCtx:   flowCtx,
		spec:      spec,
		output:    output,
		scatterer: scatterer,
		// Large enough so that it never blocks.
		doneScatterCh:     make(chan entryNode, numEntries),
		routingDatumCache: make(map[roachpb.NodeID]rowenc.EncDatum),
	}
	if err := ssp.Init(ssp, post, splitAndScatterOutputTypes, flowCtx, processorID, output, nil, /* memMonitor */
		execinfra.ProcStateOpts{
			InputsToDrain: nil, // there are no inputs to drain
			TrailingMetaCallback: func(context.Context) []execinfrapb.ProducerMetadata {
				ssp.close()
				return nil
			},
		}); err != nil {
		return nil, err
	}
	return ssp, nil
}

// Start is part of the RowSource interface.
func (ssp *splitAndScatterProcessor) Start(ctx context.Context) {
	ctx = ssp.StartInternal(ctx, splitAndScatterProcessorName)
	go func() {
		// Note that the loop over doneScatterCh in Next should prevent this
		// goroutine from leaking when there are no errors. However, if that loop
		// needs to exit early, runSplitAndScatter's context will be canceled.
		scatterCtx, stopScattering := context.WithCancel(ctx)
		ssp.stopScattering = stopScattering

		defer close(ssp.doneScatterCh)
		ssp.scatterErr = ssp.runSplitAndScatter(scatterCtx, ssp.flowCtx, &ssp.spec, ssp.scatterer)
	}()
}

type entryNode struct {
	entry execinfrapb.RestoreSpanEntry
	node  roachpb.NodeID
}

// Next implements the execinfra.RowSource interface.
func (ssp *splitAndScatterProcessor) Next() (rowenc.EncDatumRow, *execinfrapb.ProducerMetadata) {
	if ssp.State != execinfra.StateRunning {
		return nil, ssp.DrainHelper()
	}

	scatteredEntry, ok := <-ssp.doneScatterCh
	if ok {
		entry := scatteredEntry.entry
		entryBytes, err := protoutil.Marshal(&entry)
		if err != nil {
			ssp.MoveToDraining(err)
			return nil, ssp.DrainHelper()
		}

		// The routing datums informs the router which output stream should be used.
		routingDatum, ok := ssp.routingDatumCache[scatteredEntry.node]
		if !ok {
			routingDatum, _ = routingDatumsForNode(scatteredEntry.node)
			ssp.routingDatumCache[scatteredEntry.node] = routingDatum
		}

		row := rowenc.EncDatumRow{
			routingDatum,
			rowenc.DatumToEncDatum(types.Bytes, tree.NewDBytes(tree.DBytes(entryBytes))),
		}
		return row, nil
	}

	if ssp.scatterErr != nil {
		ssp.MoveToDraining(ssp.scatterErr)
		return nil, ssp.DrainHelper()
	}

	ssp.MoveToDraining(nil /* error */)
	return nil, ssp.DrainHelper()
}

// ConsumerClosed is part of the RowSource interface.
func (ssp *splitAndScatterProcessor) ConsumerClosed() {
	// The consumer is done, Next() will not be called again.
	ssp.close()
}

// close stops the production workers. This needs to be called if the consumer
// runs into an error and stops consuming scattered entries to make sure we
// don't leak goroutines.
func (ssp *splitAndScatterProcessor) close() {
	if ssp.InternalClose() {
		if ssp.stopScattering != nil {
			ssp.stopScattering()
		}
	}
}

func (ssp *splitAndScatterProcessor) runSplitAndScatter(
	ctx context.Context,
	flowCtx *execinfra.FlowCtx,
	spec *execinfrapb.SplitAndScatterSpec,
	scatterer splitAndScatterer,
) error {
	db := flowCtx.Cfg.DB
	kr, err := storageccl.MakeKeyRewriterFromRekeys(flowCtx.Codec(), spec.Rekeys)
	if err != nil {
		return err
	}
	g := ctxgroup.WithContext(ctx)

	importSpanChunksCh := make(chan []execinfrapb.RestoreSpanEntry)
	g.GoCtx(func(ctx context.Context) error {
		defer close(importSpanChunksCh)
		for _, importSpanChunk := range spec.Chunks {
			_, err := scatterer.splitAndScatterKey(ctx, flowCtx.Codec(), db, kr, importSpanChunk.Entries[0].Span.Key, true /* randomizeLeases */)
			if err != nil {
				return err
			}

			select {
			case <-ctx.Done():
				return ctx.Err()
			case importSpanChunksCh <- importSpanChunk.Entries:
			}
		}
		return nil
	})

	// TODO(pbardea): This tries to cover for a bad scatter by having 2 * the
	// number of nodes in the cluster. Is it necessary?
	splitScatterWorkers := 2
	for worker := 0; worker < splitScatterWorkers; worker++ {
		g.GoCtx(func(ctx context.Context) error {
			for importSpanChunk := range importSpanChunksCh {
				log.Infof(ctx, "processing a chunk")
				for _, importSpan := range importSpanChunk {
					log.Infof(ctx, "processing a span [%s,%s)", importSpan.Span.Key, importSpan.Span.EndKey)
					destination, err := scatterer.splitAndScatterKey(ctx, flowCtx.Codec(), db, kr, importSpan.Span.Key, false /* randomizeLeases */)
					if err != nil {
						return err
					}

					scatteredEntry := entryNode{
						entry: importSpan,
						node:  destination,
					}

					select {
					case <-ctx.Done():
						return ctx.Err()
					case ssp.doneScatterCh <- scatteredEntry:
					}
				}
			}
			return nil
		})
	}

	return g.Wait()
}

func routingDatumsForNode(nodeID roachpb.NodeID) (rowenc.EncDatum, rowenc.EncDatum) {
	routingBytes := roachpb.Key(fmt.Sprintf("node%d", nodeID))
	startDatum := rowenc.DatumToEncDatum(types.Bytes, tree.NewDBytes(tree.DBytes(routingBytes)))
	endDatum := rowenc.DatumToEncDatum(types.Bytes, tree.NewDBytes(tree.DBytes(routingBytes.Next())))
	return startDatum, endDatum
}

// routingSpanForNode provides the mapping to be used during distsql planning
// when setting up the output router.
func routingSpanForNode(nodeID roachpb.NodeID) ([]byte, []byte, error) {
	var alloc rowenc.DatumAlloc
	startDatum, endDatum := routingDatumsForNode(nodeID)

	startBytes, endBytes := make([]byte, 0), make([]byte, 0)
	startBytes, err := startDatum.Encode(splitAndScatterOutputTypes[0], &alloc, descpb.DatumEncoding_ASCENDING_KEY, startBytes)
	if err != nil {
		return nil, nil, err
	}
	endBytes, err = endDatum.Encode(splitAndScatterOutputTypes[0], &alloc, descpb.DatumEncoding_ASCENDING_KEY, endBytes)
	if err != nil {
		return nil, nil, err
	}
	return startBytes, endBytes, nil
}

func init() {
	rowexec.NewSplitAndScatterProcessor = newSplitAndScatterProcessor
}
