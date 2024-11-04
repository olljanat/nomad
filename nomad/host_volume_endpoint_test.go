// Copyright (c) HashiCorp, Inc.
// SPDX-License-Identifier: BUSL-1.1

package nomad

import (
	"context"
	"errors"
	"io"
	"sync"
	"testing"
	"time"

	msgpackrpc "github.com/hashicorp/net-rpc-msgpackrpc/v2"
	"github.com/hashicorp/nomad/ci"
	cstructs "github.com/hashicorp/nomad/client/structs"
	"github.com/hashicorp/nomad/nomad/mock"
	"github.com/hashicorp/nomad/nomad/structs"
	"github.com/hashicorp/nomad/testutil"
	"github.com/hashicorp/yamux"
	"github.com/shoenig/test/must"
)

func hostVolumeTestSetup(t *testing.T) (*Server, *mockHostVolumeClientEndpoint, string, string) {
	t.Helper()

	srv, _, cleanupSrv := TestACLServer(t, nil)
	t.Cleanup(cleanupSrv)
	testutil.WaitForLeader(t, srv.RPC)

	mockClientEndpoint := &mockHostVolumeClientEndpoint{}

	// c1, cleanupC1 := client.TestClientWithRPCs(t,
	// 	func(c *config.Config) {
	// 		c.Servers = []string{srv.config.RPCAddr.String()}
	// 		c.Node.NodePool = "prod"
	// 	},
	// 	map[string]any{"HostVolume": mockClientEndpoint},
	// )
	// t.Cleanup(func() { cleanupC1() })

	index := uint64(1001)

	node := mock.Node()
	node.Attributes["nomad.version"] = "1.9.1" // TODO(1.10.0): we should version-gate these
	node.NodePool = "prod"
	srv.fsm.State().UpsertNode(structs.MsgTypeTestSetup, index, node)

	buf := io.ByteReader
	c := yamux.Client(conn io.ReadWriteCloser, config *yamux.Config)
	srv.addNodeConn(&RPCContext{
		NodeID:  node.ID,
		Session: &yamux.Session{},
	})

	token := mock.CreatePolicyAndToken(t, srv.fsm.State(), index, "volume-manager",
		`namespace "apps" { capabilities = ["host-volume-register"] }
         node { policy = "read" }
`,
	)
	index++
	otherToken := mock.CreatePolicyAndToken(t, srv.fsm.State(), index, "other",
		`namespace "foo" { capabilities = ["host-volume-register"] }
         node { policy = "read" }
`,
	)
	index++
	ns := mock.Namespace()
	ns.Name = "apps"
	srv.fsm.State().UpsertNamespaces(index, []*structs.Namespace{ns})

	// testutil.WaitForClientStatusWithToken(t,
	// 	srv.RPC, c1.NodeID(), srv.Region(), structs.NodeStatusReady, token.SecretID)

	return srv, mockClientEndpoint, token.SecretID, otherToken.SecretID
}

func TestHostVolumeEndpoint_CRUD(t *testing.T) {
	ci.Parallel(t)

	srv, mockClientEndpoint, token, otherToken := hostVolumeTestSetup(t)
	codec := rpcClient(t, srv)

	req := &structs.HostVolumeCreateRequest{
		Volumes: []*structs.HostVolume{},
		WriteRequest: structs.WriteRequest{
			Region:    srv.Region(),
			Namespace: "invalid",
			AuthToken: token},
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
	expectIndex := resp.Index

	getReq := &structs.HostVolumeGetRequest{
		ID: volID,
		QueryOptions: structs.QueryOptions{
			Region:    srv.Region(),
			Namespace: "apps",
			AuthToken: otherToken,
		},
	}
	var getResp structs.HostVolumeGetResponse
	err = msgpackrpc.CallWithCodec(codec, "HostVolume.Get", getReq, &getResp)
	must.EqError(t, err, "Permission denied")

	getReq.AuthToken = token
	err = msgpackrpc.CallWithCodec(codec, "HostVolume.Get", getReq, &getResp)
	must.NoError(t, err)
	must.NotNil(t, getResp.Volume)

	next := getResp.Volume.Copy()
	next.RequestedCapacityMax = 300000
	registerReq := &structs.HostVolumeRegisterRequest{
		Volumes: []*structs.HostVolume{next},
		WriteRequest: structs.WriteRequest{
			Region:    srv.Region(),
			Namespace: "apps",
			AuthToken: token},
	}

	mockClientEndpoint.setCreate(nil,
		errors.New("should not call this endpoint on register RPC"))

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)
	volCh := make(chan *structs.HostVolume)
	errCh := make(chan error)

	getReq.MinQueryIndex = expectIndex

	go func() {
		codec := rpcClient(t, srv)
		var getResp structs.HostVolumeGetResponse
		err := msgpackrpc.CallWithCodec(codec, "HostVolume.Get", getReq, &getResp)
		if err != nil {
			errCh <- err
		}
		volCh <- getResp.Volume
	}()

	var registerResp structs.HostVolumeRegisterResponse
	err = msgpackrpc.CallWithCodec(codec, "HostVolume.Register", registerReq, &registerResp)
	must.NoError(t, err)

	select {
	case <-ctx.Done():
		t.Fatal("timeout or cancelled")
	case vol := <-volCh:
		must.Greater(t, expectIndex, vol.ModifyIndex)
	case err := <-errCh:
		t.Fatalf("unexpected error: %v", err)
	}

	delReq := &structs.HostVolumeDeleteRequest{
		VolumeIDs: []string{volID},
		WriteRequest: structs.WriteRequest{
			Region:    srv.Region(),
			Namespace: "apps",
			AuthToken: token},
	}
	var delResp structs.HostVolumeDeleteResponse
	err = msgpackrpc.CallWithCodec(codec, "HostVolume.Delete", delReq, &delResp)
	must.NoError(t, err)

	err = msgpackrpc.CallWithCodec(codec, "HostVolume.Get", getReq, &getResp)
	must.NoError(t, err)
	must.Nil(t, getResp.Volume)
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
	nextDeleteErr      error
}

func (v *mockHostVolumeClientEndpoint) setCreate(
	resp *cstructs.ClientHostVolumeCreateResponse, err error) {
	v.lock.Lock()
	defer v.lock.Unlock()
	v.nextCreateResponse = resp
	v.nextCreateErr = err
}

func (v *mockHostVolumeClientEndpoint) setDelete(err error) {
	v.lock.Lock()
	defer v.lock.Unlock()
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
	return v.nextDeleteErr
}
