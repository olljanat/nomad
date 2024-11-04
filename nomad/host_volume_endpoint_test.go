// Copyright (c) HashiCorp, Inc.
// SPDX-License-Identifier: BUSL-1.1

package nomad

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	msgpackrpc "github.com/hashicorp/net-rpc-msgpackrpc/v2"
	"github.com/hashicorp/nomad/ci"
	"github.com/hashicorp/nomad/client"
	"github.com/hashicorp/nomad/client/config"
	cstructs "github.com/hashicorp/nomad/client/structs"
	"github.com/hashicorp/nomad/nomad/mock"
	"github.com/hashicorp/nomad/nomad/structs"
	"github.com/hashicorp/nomad/testutil"
	"github.com/shoenig/test/must"
)

func TestHostVolumeEndpoint_CRUD(t *testing.T) {
	ci.Parallel(t)

	srv, _, cleanupSrv := TestACLServer(t, nil)
	t.Cleanup(cleanupSrv)

	codec := rpcClient(t, srv)
	testutil.WaitForLeader(t, srv.RPC)

	mockClientEndpoint := &mockHostVolumeClientEndpoint{}

	c1, cleanupC1 := client.TestClientWithRPCs(t,
		func(c *config.Config) {
			c.Servers = []string{srv.config.RPCAddr.String()}
			c.Node.NodePool = "prod"
		},
		map[string]any{"HostVolume": mockClientEndpoint},
	)
	t.Cleanup(func() { cleanupC1() })

	index := uint64(1001)
	token := mock.CreatePolicyAndToken(t, srv.fsm.State(), index, "volume-manager",
		`namespace "apps" { capabilities = ["host-volume-register"] }
         node { policy= "read" }
`,
	)
	index++
	otherToken := mock.CreatePolicyAndToken(t, srv.fsm.State(), index, "other",
		`namespace "foo" { capabilities = ["host-volume-register"] }
         node { policy= "read" }
`,
	)
	index++
	ns := mock.Namespace()
	ns.Name = "apps"
	srv.fsm.State().UpsertNamespaces(index, []*structs.Namespace{ns})

	testutil.WaitForClientStatusWithToken(t,
		srv.RPC, c1.NodeID(), srv.Region(), structs.NodeStatusReady, token.SecretID)

	req := &structs.HostVolumeCreateRequest{
		Volumes: []*structs.HostVolume{},
		WriteRequest: structs.WriteRequest{
			Region:    srv.Region(),
			Namespace: "invalid",
			AuthToken: token.SecretID},
	}

	var resp structs.HostVolumeCreateResponse
	err := msgpackrpc.CallWithCodec(codec, "HostVolume.Create", req, &resp)
	must.EqError(t, err, "Permission denied")

	// TODO(1.10.0): once validation logic is in place, fully test here
	req.Namespace = "apps"
	err = msgpackrpc.CallWithCodec(codec, "HostVolume.Create", req, &resp)
	must.EqError(t, err, "missing volume definition")

	mockClientEndpoint.setCreate(&cstructs.ClientHostVolumeCreateResponse{
		HostPath: "/var/nomad/alloc_mounts/foo",
		Capacity: 150000,
	}, nil)

	vol1 := mock.HostVolumeRequest()
	vol1.NodePool = "prod"
	req.Volumes = []*structs.HostVolume{vol1}
	err = msgpackrpc.CallWithCodec(codec, "HostVolume.Create", req, &resp)
	must.NoError(t, err)
	must.Len(t, 1, resp.Volumes)
	volID := resp.Volumes[0].ID
	expectIndex := resp.Volumes[0].CreateIndex

	getReq := &structs.HostVolumeGetRequest{
		ID: volID,
		QueryOptions: structs.QueryOptions{
			Region:    srv.Region(),
			Namespace: "apps",
			AuthToken: otherToken.SecretID,
		},
	}
	var getResp structs.HostVolumeGetResponse
	err = msgpackrpc.CallWithCodec(codec, "HostVolume.Get", getReq, &getResp)
	must.EqError(t, err, "Permission denied")

	getReq.AuthToken = token.SecretID
	err = msgpackrpc.CallWithCodec(codec, "HostVolume.Get", getReq, &getResp)
	must.NoError(t, err)
	must.NotNil(t, getResp.Volume)
	getResp.Volume.Capacity = 150000
	getResp.Volume.ID = volID
	getResp.Volume.CreateIndex = expectIndex

	unblockMinIndex := expectIndex + 1
	getReq.MinQueryIndex = unblockMinIndex
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)
	ch := make(chan *structs.HostVolume)

	go func() {
		err = msgpackrpc.CallWithCodec(codec, "HostVolume.Get", getReq, &getResp)
		must.NoError(t, err)
		ch <- getResp.Volume
	}()

	select {
	case <-ctx.Done():
		t.Fatal("timeout")
	case vol := <-ch:
		fmt.Println("vol.ModifyIndex:", vol.ModifyIndex, "unblockMinIndex", unblockMinIndex)
		must.Greater(t, unblockMinIndex, vol.ModifyIndex)
	}

}

func TestHostVolumeEndpoint_List_Filters(t *testing.T) {

}

func TestHostVolumeEndpoint_List_BlockingQuery(t *testing.T) {

}

// mockHostVolumeClientEndpoint models client RPCs that have side-effects on the
// client host
type mockHostVolumeClientEndpoint struct {
	lock               sync.Mutex
	nextCreateResponse *cstructs.ClientHostVolumeCreateResponse
	nextCreateErr      error
	nextDeleteResponse *cstructs.ClientHostVolumeDeleteResponse
	nextDeleteErr      error
}

func (v *mockHostVolumeClientEndpoint) setCreate(
	resp *cstructs.ClientHostVolumeCreateResponse, err error) {
	v.lock.Lock()
	defer v.lock.Unlock()
	v.nextCreateResponse = resp
	v.nextCreateErr = err
}

func (v *mockHostVolumeClientEndpoint) setDelete(
	resp *cstructs.ClientHostVolumeDeleteResponse, err error) {
	v.lock.Lock()
	defer v.lock.Unlock()
	v.nextDeleteResponse = resp
	v.nextDeleteErr = err
}

func (v *mockHostVolumeClientEndpoint) Create(
	req *cstructs.ClientHostVolumeCreateRequest,
	resp *cstructs.ClientHostVolumeCreateResponse) error {
	v.lock.Lock()
	defer v.lock.Unlock()
	*resp = *v.nextCreateResponse
	return v.nextCreateErr
}

func (v *mockHostVolumeClientEndpoint) Delete(
	req *cstructs.ClientHostVolumeDeleteRequest,
	resp *cstructs.ClientHostVolumeDeleteResponse) error {
	v.lock.Lock()
	defer v.lock.Unlock()
	*resp = *v.nextDeleteResponse
	return v.nextDeleteErr
}
