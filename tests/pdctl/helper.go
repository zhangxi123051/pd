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

package pdctl

import (
	"bytes"
	"context"
	"fmt"
	"io/ioutil"
	"os"
	"path/filepath"
	"sort"

	"github.com/gogo/protobuf/proto"
	"github.com/pingcap/check"
	"github.com/pingcap/kvproto/pkg/metapb"
	"github.com/pingcap/kvproto/pkg/pdpb"
	"github.com/pingcap/pd/v4/server"
	"github.com/pingcap/pd/v4/server/api"
	"github.com/pingcap/pd/v4/server/cluster"
	"github.com/pingcap/pd/v4/server/core"
	"github.com/pingcap/pd/v4/tests"
	"github.com/pingcap/pd/v4/tools/pd-ctl/pdctl"
	ctl "github.com/pingcap/pd/v4/tools/pd-ctl/pdctl"
	"github.com/pingcap/pd/v4/tools/pd-ctl/pdctl/command"
	"github.com/spf13/cobra"
)

// InitCommand is used to initialize command.
func InitCommand() *cobra.Command {
	commandFlags := pdctl.CommandFlags{}
	rootCmd := &cobra.Command{}
	rootCmd.PersistentFlags().StringVarP(&commandFlags.URL, "pd", "u", "", "")
	rootCmd.Flags().StringVar(&commandFlags.CAPath, "cacert", "", "")
	rootCmd.Flags().StringVar(&commandFlags.CertPath, "cert", "", "")
	rootCmd.Flags().StringVar(&commandFlags.KeyPath, "key", "", "")
	rootCmd.AddCommand(
		command.NewConfigCommand(),
		command.NewRegionCommand(),
		command.NewStoreCommand(),
		command.NewStoresCommand(),
		command.NewMemberCommand(),
		command.NewExitCommand(),
		command.NewLabelCommand(),
		command.NewPingCommand(),
		command.NewOperatorCommand(),
		command.NewSchedulerCommand(),
		command.NewTSOCommand(),
		command.NewHotSpotCommand(),
		command.NewClusterCommand(),
		command.NewHealthCommand(),
		command.NewLogCommand(),
		command.NewPluginCommand(),
		command.NewComponentCommand(),
	)
	return rootCmd
}

// ExecuteCommandC is used for test purpose.
func ExecuteCommandC(root *cobra.Command, args ...string) (c *cobra.Command, output []byte, err error) {
	buf := new(bytes.Buffer)
	root.SetOutput(buf)
	root.SetArgs(args)

	c, err = root.ExecuteC()
	return c, buf.Bytes(), err
}

// CheckStoresInfo is used to check the test results.
func CheckStoresInfo(c *check.C, stores []*api.StoreInfo, want []*metapb.Store) {
	c.Assert(len(stores), check.Equals, len(want))
	mapWant := make(map[uint64]*metapb.Store)
	for _, s := range want {
		if _, ok := mapWant[s.Id]; !ok {
			mapWant[s.Id] = s
		}
	}
	for _, s := range stores {
		obtained := proto.Clone(s.Store.Store).(*metapb.Store)
		expected := proto.Clone(mapWant[obtained.Id]).(*metapb.Store)
		// Ignore lastHeartbeat
		obtained.LastHeartbeat, expected.LastHeartbeat = 0, 0
		c.Assert(obtained, check.DeepEquals, expected)
	}
}

// CheckRegionsInfo is used to check the test results.
func CheckRegionsInfo(c *check.C, output api.RegionsInfo, expected []*core.RegionInfo) {
	c.Assert(output.Count, check.Equals, len(expected))
	got := output.Regions
	sort.Slice(got, func(i, j int) bool {
		return got[i].ID < got[j].ID
	})
	sort.Slice(expected, func(i, j int) bool {
		return expected[i].GetID() < expected[j].GetID()
	})
	for i, region := range expected {
		c.Assert(api.NewRegionInfo(region), check.DeepEquals, got[i])
	}
}

// MustPutStore is used for test purpose.
func MustPutStore(c *check.C, svr *server.Server, id uint64, state metapb.StoreState, labels []*metapb.StoreLabel) {
	_, err := svr.PutStore(context.Background(), &pdpb.PutStoreRequest{
		Header: &pdpb.RequestHeader{ClusterId: svr.ClusterID()},
		Store: &metapb.Store{
			Id:      id,
			Address: fmt.Sprintf("tikv%d", id),
			State:   state,
			Labels:  labels,
			Version: (*cluster.MinSupportedVersion(cluster.Version2_0)).String(),
		},
	})
	c.Assert(err, check.IsNil)
	_, err = svr.StoreHeartbeat(context.Background(), &pdpb.StoreHeartbeatRequest{
		Header: &pdpb.RequestHeader{ClusterId: svr.ClusterID()},
		Stats:  &pdpb.StoreStats{StoreId: id},
	})
	c.Assert(err, check.IsNil)
}

// MustPutRegion is used for test purpose.
func MustPutRegion(c *check.C, cluster *tests.TestCluster, regionID, storeID uint64, start, end []byte, opts ...core.RegionCreateOption) *core.RegionInfo {
	leader := &metapb.Peer{
		Id:      regionID,
		StoreId: storeID,
	}
	metaRegion := &metapb.Region{
		Id:          regionID,
		StartKey:    start,
		EndKey:      end,
		Peers:       []*metapb.Peer{leader},
		RegionEpoch: &metapb.RegionEpoch{ConfVer: 1, Version: 1},
	}
	r := core.NewRegionInfo(metaRegion, leader, opts...)
	err := cluster.HandleRegionHeartbeat(r)
	c.Assert(err, check.IsNil)
	return r
}

// GetEcho is used to get echo from stdout.
func GetEcho(args []string) string {
	filename := filepath.Join(os.TempDir(), "stdout")
	old := os.Stdout
	temp, _ := os.Create(filename)
	os.Stdout = temp
	ctl.Start(args)
	temp.Close()
	os.Stdout = old
	out, _ := ioutil.ReadFile(filename)
	_ = os.Remove(filename)
	return string(out)
}
