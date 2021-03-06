// Copyright 2019 PingCAP, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// See the License for the specific language governing permissions and
// limitations under the License.

package backend

import (
	"context"
	"fmt"
	"net/http"

	"github.com/pingcap/errors"
	"github.com/pingcap/kvproto/pkg/import_sstpb"
	"go.uber.org/zap"
	"golang.org/x/sync/errgroup"
	"google.golang.org/grpc"

	"github.com/pingcap/tidb-lightning/lightning/common"
	"github.com/pingcap/tidb-lightning/lightning/log"
)

// StoreState is the state of a TiKV store. The numerical value is sorted by
// the store's accessibility (Tombstone < Down < Disconnected < Offline < Up).
//
// The meaning of each state can be found from PingCAP's documentation at
// https://pingcap.com/docs/v3.0/how-to/scale/horizontally/#delete-a-node-dynamically-1
type StoreState int

const (
	// StoreStateUp means the TiKV store is in service.
	StoreStateUp StoreState = -iota
	// StoreStateOffline means the TiKV store is in the process of being taken
	// offline (but is still accessible).
	StoreStateOffline
	// StoreStateDisconnected means the TiKV store does not respond to PD.
	StoreStateDisconnected
	// StoreStateDown means the TiKV store does not respond to PD for a long
	// time (> 30 minutes).
	StoreStateDown
	// StoreStateTombstone means the TiKV store is shut down and the data has
	// been evacuated. Lightning should never interact with stores in this
	// state.
	StoreStateTombstone
)

var jsonToStoreState = map[string]StoreState{
	`"Up"`:           StoreStateUp,
	`"Offline"`:      StoreStateOffline,
	`"Disconnected"`: StoreStateDisconnected,
	`"Down"`:         StoreStateDown,
	`"Tombstone"`:    StoreStateTombstone,
}

// UnmarshalJSON implements the json.Unmarshaler interface.
func (ss *StoreState) UnmarshalJSON(content []byte) error {
	if state, ok := jsonToStoreState[string(content)]; ok {
		*ss = state
		return nil
	}
	return errors.New("Unknown store state")
}

// Store contains metadata about a TiKV store.
type Store struct {
	Address string
	Version string
	State   StoreState `json:"state_name"`
}

func withTiKVConnection(ctx context.Context, tikvAddr string, action func(import_sstpb.ImportSSTClient) error) error {
	// Connect to the ImportSST service on the given TiKV node.
	// The connection is needed for executing `action` and will be tear down
	// when this function exits.
	conn, err := grpc.DialContext(ctx, tikvAddr, grpc.WithInsecure())
	if err != nil {
		return errors.Trace(err)
	}
	defer conn.Close()

	client := import_sstpb.NewImportSSTClient(conn)
	return action(client)
}

// ForAllStores executes `action` in parallel for all TiKV stores connected to
// the given PD server.
//
// Returns the first non-nil error returned in all `action` calls. If all
// `action` returns nil, this method would return nil as well.
//
// The `minState` argument defines the minimum store state to be included in the
// result (Tombstone < Offline < Down < Disconnected < Up).
func ForAllStores(
	ctx context.Context,
	client *http.Client,
	pdAddr string,
	minState StoreState,
	action func(c context.Context, store *Store) error,
) error {
	// Go through the HTTP interface instead of gRPC so we don't need to keep
	// track of the cluster ID.
	url := fmt.Sprintf("http://%s/pd/api/v1/stores", pdAddr)

	var stores struct {
		Stores []struct {
			Store Store
		}
	}

	err := common.GetJSON(client, url, &stores)
	if err != nil {
		return err
	}

	eg, c := errgroup.WithContext(ctx)
	for _, store := range stores.Stores {
		if store.Store.State >= minState {
			s := store.Store
			eg.Go(func() error { return action(c, &s) })
		}
	}
	return eg.Wait()
}

// SwitchMode changes the TiKV node at the given address to a particular mode.
func SwitchMode(ctx context.Context, tikvAddr string, mode import_sstpb.SwitchMode) error {
	task := log.With(zap.Stringer("mode", mode)).Begin(zap.DebugLevel, "switch mode")
	err := withTiKVConnection(ctx, tikvAddr, func(client import_sstpb.ImportSSTClient) error {
		_, err := client.SwitchMode(ctx, &import_sstpb.SwitchModeRequest{
			Mode: mode,
		})
		return errors.Trace(err)
	})
	task.End(zap.WarnLevel, err)
	return err
}

// Compact performs a leveled compaction with the given minimum level.
func Compact(ctx context.Context, tikvAddr string, level int32) error {
	task := log.With(zap.Int32("level", level)).Begin(zap.InfoLevel, "compact cluster")
	err := withTiKVConnection(ctx, tikvAddr, func(client import_sstpb.ImportSSTClient) error {
		_, err := client.Compact(ctx, &import_sstpb.CompactRequest{
			OutputLevel: level,
		})
		return errors.Trace(err)
	})
	task.End(zap.ErrorLevel, err)
	return err
}
