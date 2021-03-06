package ca_test

import (
	"bytes"
	"crypto/x509"
	"fmt"
	"io/ioutil"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/cloudflare/cfssl/helpers"
	"github.com/docker/swarmkit/api"
	"github.com/docker/swarmkit/api/equality"
	"github.com/docker/swarmkit/ca"
	cautils "github.com/docker/swarmkit/ca/testutils"
	"github.com/docker/swarmkit/manager/state/store"
	"github.com/docker/swarmkit/testutils"
	"github.com/opencontainers/go-digest"
	"github.com/pkg/errors"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/net/context"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
)

var _ api.CAServer = &ca.Server{}
var _ api.NodeCAServer = &ca.Server{}

func TestGetRootCACertificate(t *testing.T) {
	tc := cautils.NewTestCA(t)
	defer tc.Stop()

	resp, err := tc.CAClients[0].GetRootCACertificate(context.Background(), &api.GetRootCACertificateRequest{})
	assert.NoError(t, err)
	assert.NotEmpty(t, resp.Certificate)
}

func TestRestartRootCA(t *testing.T) {
	tc := cautils.NewTestCA(t)
	defer tc.Stop()

	_, err := tc.NodeCAClients[0].NodeCertificateStatus(context.Background(), &api.NodeCertificateStatusRequest{NodeID: "foo"})
	assert.Error(t, err)
	assert.Equal(t, codes.NotFound, grpc.Code(err))

	tc.CAServer.Stop()
	go tc.CAServer.Run(context.Background())

	<-tc.CAServer.Ready()

	_, err = tc.NodeCAClients[0].NodeCertificateStatus(context.Background(), &api.NodeCertificateStatusRequest{NodeID: "foo"})
	assert.Error(t, err)
	assert.Equal(t, codes.NotFound, grpc.Code(err))
}

func TestIssueNodeCertificate(t *testing.T) {
	tc := cautils.NewTestCA(t)
	defer tc.Stop()

	csr, _, err := ca.GenerateNewCSR()
	assert.NoError(t, err)

	issueRequest := &api.IssueNodeCertificateRequest{CSR: csr, Token: tc.WorkerToken}
	issueResponse, err := tc.NodeCAClients[0].IssueNodeCertificate(context.Background(), issueRequest)
	assert.NoError(t, err)
	assert.NotNil(t, issueResponse.NodeID)
	assert.Equal(t, api.NodeMembershipAccepted, issueResponse.NodeMembership)

	statusRequest := &api.NodeCertificateStatusRequest{NodeID: issueResponse.NodeID}
	statusResponse, err := tc.NodeCAClients[0].NodeCertificateStatus(context.Background(), statusRequest)
	require.NoError(t, err)
	assert.Equal(t, api.IssuanceStateIssued, statusResponse.Status.State)
	assert.NotNil(t, statusResponse.Certificate.Certificate)
	assert.Equal(t, api.NodeRoleWorker, statusResponse.Certificate.Role)
}

func TestForceRotationIsNoop(t *testing.T) {
	tc := cautils.NewTestCA(t)
	defer tc.Stop()

	// Get a new Certificate issued
	csr, _, err := ca.GenerateNewCSR()
	assert.NoError(t, err)

	issueRequest := &api.IssueNodeCertificateRequest{CSR: csr, Token: tc.WorkerToken}
	issueResponse, err := tc.NodeCAClients[0].IssueNodeCertificate(context.Background(), issueRequest)
	assert.NoError(t, err)
	assert.NotNil(t, issueResponse.NodeID)
	assert.Equal(t, api.NodeMembershipAccepted, issueResponse.NodeMembership)

	// Check that the Certificate is successfully issued
	statusRequest := &api.NodeCertificateStatusRequest{NodeID: issueResponse.NodeID}
	statusResponse, err := tc.NodeCAClients[0].NodeCertificateStatus(context.Background(), statusRequest)
	require.NoError(t, err)
	assert.Equal(t, api.IssuanceStateIssued, statusResponse.Status.State)
	assert.NotNil(t, statusResponse.Certificate.Certificate)
	assert.Equal(t, api.NodeRoleWorker, statusResponse.Certificate.Role)

	// Update the certificate status to IssuanceStateRotate which should be a server-side noop
	err = tc.MemoryStore.Update(func(tx store.Tx) error {
		// Attempt to retrieve the node with nodeID
		node := store.GetNode(tx, issueResponse.NodeID)
		assert.NotNil(t, node)

		node.Certificate.Status.State = api.IssuanceStateRotate
		return store.UpdateNode(tx, node)
	})
	assert.NoError(t, err)

	// Wait a bit and check that the certificate hasn't changed/been reissued
	time.Sleep(250 * time.Millisecond)

	statusNewResponse, err := tc.NodeCAClients[0].NodeCertificateStatus(context.Background(), statusRequest)
	require.NoError(t, err)
	assert.Equal(t, statusResponse.Certificate.Certificate, statusNewResponse.Certificate.Certificate)
	assert.Equal(t, api.IssuanceStateRotate, statusNewResponse.Certificate.Status.State)
	assert.Equal(t, api.NodeRoleWorker, statusNewResponse.Certificate.Role)
}

func TestIssueNodeCertificateBrokenCA(t *testing.T) {
	if !cautils.External {
		t.Skip("test only applicable for external CA configuration")
	}

	tc := cautils.NewTestCA(t)
	defer tc.Stop()

	csr, _, err := ca.GenerateNewCSR()
	assert.NoError(t, err)

	tc.ExternalSigningServer.Flake()

	go func() {
		time.Sleep(250 * time.Millisecond)
		tc.ExternalSigningServer.Deflake()
	}()
	issueRequest := &api.IssueNodeCertificateRequest{CSR: csr, Token: tc.WorkerToken}
	issueResponse, err := tc.NodeCAClients[0].IssueNodeCertificate(context.Background(), issueRequest)
	assert.NoError(t, err)
	assert.NotNil(t, issueResponse.NodeID)
	assert.Equal(t, api.NodeMembershipAccepted, issueResponse.NodeMembership)

	statusRequest := &api.NodeCertificateStatusRequest{NodeID: issueResponse.NodeID}
	statusResponse, err := tc.NodeCAClients[0].NodeCertificateStatus(context.Background(), statusRequest)
	require.NoError(t, err)
	assert.Equal(t, api.IssuanceStateIssued, statusResponse.Status.State)
	assert.NotNil(t, statusResponse.Certificate.Certificate)
	assert.Equal(t, api.NodeRoleWorker, statusResponse.Certificate.Role)

}

func TestIssueNodeCertificateWithInvalidCSR(t *testing.T) {
	tc := cautils.NewTestCA(t)
	defer tc.Stop()

	issueRequest := &api.IssueNodeCertificateRequest{CSR: []byte("random garbage"), Token: tc.WorkerToken}
	issueResponse, err := tc.NodeCAClients[0].IssueNodeCertificate(context.Background(), issueRequest)
	assert.NoError(t, err)
	assert.NotNil(t, issueResponse.NodeID)
	assert.Equal(t, api.NodeMembershipAccepted, issueResponse.NodeMembership)

	statusRequest := &api.NodeCertificateStatusRequest{NodeID: issueResponse.NodeID}
	statusResponse, err := tc.NodeCAClients[0].NodeCertificateStatus(context.Background(), statusRequest)
	require.NoError(t, err)
	assert.Equal(t, api.IssuanceStateFailed, statusResponse.Status.State)
	assert.Contains(t, statusResponse.Status.Err, "CSR Decode failed")
	assert.Nil(t, statusResponse.Certificate.Certificate)
}

func TestIssueNodeCertificateWorkerRenewal(t *testing.T) {
	tc := cautils.NewTestCA(t)
	defer tc.Stop()

	csr, _, err := ca.GenerateNewCSR()
	assert.NoError(t, err)

	role := api.NodeRoleWorker
	issueRequest := &api.IssueNodeCertificateRequest{CSR: csr, Role: role}
	issueResponse, err := tc.NodeCAClients[1].IssueNodeCertificate(context.Background(), issueRequest)
	assert.NoError(t, err)
	assert.NotNil(t, issueResponse.NodeID)
	assert.Equal(t, api.NodeMembershipAccepted, issueResponse.NodeMembership)

	statusRequest := &api.NodeCertificateStatusRequest{NodeID: issueResponse.NodeID}
	statusResponse, err := tc.NodeCAClients[1].NodeCertificateStatus(context.Background(), statusRequest)
	require.NoError(t, err)
	assert.Equal(t, api.IssuanceStateIssued, statusResponse.Status.State)
	assert.NotNil(t, statusResponse.Certificate.Certificate)
	assert.Equal(t, role, statusResponse.Certificate.Role)
}

func TestIssueNodeCertificateManagerRenewal(t *testing.T) {
	tc := cautils.NewTestCA(t)
	defer tc.Stop()

	csr, _, err := ca.GenerateNewCSR()
	assert.NoError(t, err)
	assert.NotNil(t, csr)

	role := api.NodeRoleManager
	issueRequest := &api.IssueNodeCertificateRequest{CSR: csr, Role: role}
	issueResponse, err := tc.NodeCAClients[2].IssueNodeCertificate(context.Background(), issueRequest)
	require.NoError(t, err)
	assert.NotNil(t, issueResponse.NodeID)
	assert.Equal(t, api.NodeMembershipAccepted, issueResponse.NodeMembership)

	statusRequest := &api.NodeCertificateStatusRequest{NodeID: issueResponse.NodeID}
	statusResponse, err := tc.NodeCAClients[2].NodeCertificateStatus(context.Background(), statusRequest)
	require.NoError(t, err)
	assert.Equal(t, api.IssuanceStateIssued, statusResponse.Status.State)
	assert.NotNil(t, statusResponse.Certificate.Certificate)
	assert.Equal(t, role, statusResponse.Certificate.Role)
}

func TestIssueNodeCertificateWorkerFromDifferentOrgRenewal(t *testing.T) {
	tc := cautils.NewTestCA(t)
	defer tc.Stop()

	csr, _, err := ca.GenerateNewCSR()
	assert.NoError(t, err)

	// Since we're using a client that has a different Organization, this request will be treated
	// as a new certificate request, not allowing auto-renewal. Therefore, the request will fail.
	issueRequest := &api.IssueNodeCertificateRequest{CSR: csr}
	_, err = tc.NodeCAClients[3].IssueNodeCertificate(context.Background(), issueRequest)
	assert.Error(t, err)
}

func TestNodeCertificateRenewalsDoNotRequireToken(t *testing.T) {
	tc := cautils.NewTestCA(t)
	defer tc.Stop()

	csr, _, err := ca.GenerateNewCSR()
	assert.NoError(t, err)

	role := api.NodeRoleManager
	issueRequest := &api.IssueNodeCertificateRequest{CSR: csr, Role: role}
	issueResponse, err := tc.NodeCAClients[2].IssueNodeCertificate(context.Background(), issueRequest)
	assert.NoError(t, err)
	assert.NotNil(t, issueResponse.NodeID)
	assert.Equal(t, api.NodeMembershipAccepted, issueResponse.NodeMembership)

	statusRequest := &api.NodeCertificateStatusRequest{NodeID: issueResponse.NodeID}
	statusResponse, err := tc.NodeCAClients[2].NodeCertificateStatus(context.Background(), statusRequest)
	assert.NoError(t, err)
	assert.Equal(t, api.IssuanceStateIssued, statusResponse.Status.State)
	assert.NotNil(t, statusResponse.Certificate.Certificate)
	assert.Equal(t, role, statusResponse.Certificate.Role)

	role = api.NodeRoleWorker
	issueRequest = &api.IssueNodeCertificateRequest{CSR: csr, Role: role}
	issueResponse, err = tc.NodeCAClients[1].IssueNodeCertificate(context.Background(), issueRequest)
	require.NoError(t, err)
	assert.NotNil(t, issueResponse.NodeID)
	assert.Equal(t, api.NodeMembershipAccepted, issueResponse.NodeMembership)

	statusRequest = &api.NodeCertificateStatusRequest{NodeID: issueResponse.NodeID}
	statusResponse, err = tc.NodeCAClients[2].NodeCertificateStatus(context.Background(), statusRequest)
	require.NoError(t, err)
	assert.Equal(t, api.IssuanceStateIssued, statusResponse.Status.State)
	assert.NotNil(t, statusResponse.Certificate.Certificate)
	assert.Equal(t, role, statusResponse.Certificate.Role)
}

func TestNewNodeCertificateRequiresToken(t *testing.T) {
	t.Parallel()

	tc := cautils.NewTestCA(t)
	defer tc.Stop()

	csr, _, err := ca.GenerateNewCSR()
	assert.NoError(t, err)

	// Issuance fails if no secret is provided
	role := api.NodeRoleManager
	issueRequest := &api.IssueNodeCertificateRequest{CSR: csr, Role: role}
	_, err = tc.NodeCAClients[0].IssueNodeCertificate(context.Background(), issueRequest)
	assert.EqualError(t, err, "rpc error: code = 3 desc = A valid join token is necessary to join this cluster")

	role = api.NodeRoleWorker
	issueRequest = &api.IssueNodeCertificateRequest{CSR: csr, Role: role}
	_, err = tc.NodeCAClients[0].IssueNodeCertificate(context.Background(), issueRequest)
	assert.EqualError(t, err, "rpc error: code = 3 desc = A valid join token is necessary to join this cluster")

	// Issuance fails if wrong secret is provided
	role = api.NodeRoleManager
	issueRequest = &api.IssueNodeCertificateRequest{CSR: csr, Role: role, Token: "invalid-secret"}
	_, err = tc.NodeCAClients[0].IssueNodeCertificate(context.Background(), issueRequest)
	assert.EqualError(t, err, "rpc error: code = 3 desc = A valid join token is necessary to join this cluster")

	role = api.NodeRoleWorker
	issueRequest = &api.IssueNodeCertificateRequest{CSR: csr, Role: role, Token: "invalid-secret"}
	_, err = tc.NodeCAClients[0].IssueNodeCertificate(context.Background(), issueRequest)
	assert.EqualError(t, err, "rpc error: code = 3 desc = A valid join token is necessary to join this cluster")

	// Issuance succeeds if correct token is provided
	role = api.NodeRoleManager
	issueRequest = &api.IssueNodeCertificateRequest{CSR: csr, Role: role, Token: tc.ManagerToken}
	_, err = tc.NodeCAClients[0].IssueNodeCertificate(context.Background(), issueRequest)
	assert.NoError(t, err)

	role = api.NodeRoleWorker
	issueRequest = &api.IssueNodeCertificateRequest{CSR: csr, Role: role, Token: tc.WorkerToken}
	_, err = tc.NodeCAClients[0].IssueNodeCertificate(context.Background(), issueRequest)
	assert.NoError(t, err)

	// Rotate manager and worker tokens
	var (
		newManagerToken string
		newWorkerToken  string
	)
	assert.NoError(t, tc.MemoryStore.Update(func(tx store.Tx) error {
		clusters, _ := store.FindClusters(tx, store.ByName(store.DefaultClusterName))
		newWorkerToken = ca.GenerateJoinToken(&tc.RootCA)
		clusters[0].RootCA.JoinTokens.Worker = newWorkerToken
		newManagerToken = ca.GenerateJoinToken(&tc.RootCA)
		clusters[0].RootCA.JoinTokens.Manager = newManagerToken
		return store.UpdateCluster(tx, clusters[0])
	}))

	// updating the join token may take a little bit in order to register on the CA server, so poll
	assert.NoError(t, testutils.PollFunc(nil, func() error {
		// Old token should fail
		role = api.NodeRoleManager
		issueRequest = &api.IssueNodeCertificateRequest{CSR: csr, Role: role, Token: tc.ManagerToken}
		_, err = tc.NodeCAClients[0].IssueNodeCertificate(context.Background(), issueRequest)
		if err == nil {
			return fmt.Errorf("join token not updated yet")
		}
		return nil
	}))

	// Old token should fail
	assert.EqualError(t, err, "rpc error: code = 3 desc = A valid join token is necessary to join this cluster")

	role = api.NodeRoleWorker
	issueRequest = &api.IssueNodeCertificateRequest{CSR: csr, Role: role, Token: tc.WorkerToken}
	_, err = tc.NodeCAClients[0].IssueNodeCertificate(context.Background(), issueRequest)
	assert.EqualError(t, err, "rpc error: code = 3 desc = A valid join token is necessary to join this cluster")

	// New token should succeed
	role = api.NodeRoleManager
	issueRequest = &api.IssueNodeCertificateRequest{CSR: csr, Role: role, Token: newManagerToken}
	_, err = tc.NodeCAClients[0].IssueNodeCertificate(context.Background(), issueRequest)
	assert.NoError(t, err)

	role = api.NodeRoleWorker
	issueRequest = &api.IssueNodeCertificateRequest{CSR: csr, Role: role, Token: newWorkerToken}
	_, err = tc.NodeCAClients[0].IssueNodeCertificate(context.Background(), issueRequest)
	assert.NoError(t, err)
}

func TestNewNodeCertificateBadToken(t *testing.T) {
	tc := cautils.NewTestCA(t)
	defer tc.Stop()

	csr, _, err := ca.GenerateNewCSR()
	assert.NoError(t, err)

	// Issuance fails if wrong secret is provided
	role := api.NodeRoleManager
	issueRequest := &api.IssueNodeCertificateRequest{CSR: csr, Role: role, Token: "invalid-secret"}
	_, err = tc.NodeCAClients[0].IssueNodeCertificate(context.Background(), issueRequest)
	assert.EqualError(t, err, "rpc error: code = 3 desc = A valid join token is necessary to join this cluster")

	role = api.NodeRoleWorker
	issueRequest = &api.IssueNodeCertificateRequest{CSR: csr, Role: role, Token: "invalid-secret"}
	_, err = tc.NodeCAClients[0].IssueNodeCertificate(context.Background(), issueRequest)
	assert.EqualError(t, err, "rpc error: code = 3 desc = A valid join token is necessary to join this cluster")
}

func TestGetUnlockKey(t *testing.T) {
	t.Parallel()

	tc := cautils.NewTestCA(t)
	defer tc.Stop()

	var cluster *api.Cluster
	tc.MemoryStore.View(func(tx store.ReadTx) {
		clusters, err := store.FindClusters(tx, store.ByName(store.DefaultClusterName))
		require.NoError(t, err)
		cluster = clusters[0]
	})

	resp, err := tc.CAClients[0].GetUnlockKey(context.Background(), &api.GetUnlockKeyRequest{})
	require.NoError(t, err)
	require.Nil(t, resp.UnlockKey)
	require.Equal(t, cluster.Meta.Version, resp.Version)

	// Update the unlock key
	require.NoError(t, tc.MemoryStore.Update(func(tx store.Tx) error {
		cluster = store.GetCluster(tx, cluster.ID)
		cluster.Spec.EncryptionConfig.AutoLockManagers = true
		cluster.UnlockKeys = []*api.EncryptionKey{{
			Subsystem: ca.ManagerRole,
			Key:       []byte("secret"),
		}}
		return store.UpdateCluster(tx, cluster)
	}))

	tc.MemoryStore.View(func(tx store.ReadTx) {
		cluster = store.GetCluster(tx, cluster.ID)
	})

	require.NoError(t, testutils.PollFuncWithTimeout(nil, func() error {
		resp, err = tc.CAClients[0].GetUnlockKey(context.Background(), &api.GetUnlockKeyRequest{})
		if err != nil {
			return fmt.Errorf("get unlock key: %v", err)
		}
		if !bytes.Equal(resp.UnlockKey, []byte("secret")) {
			return fmt.Errorf("secret hasn't rotated yet")
		}
		if cluster.Meta.Version.Index > resp.Version.Index {
			return fmt.Errorf("hasn't updated to the right version yet")
		}
		return nil
	}, 250*time.Millisecond))
}

type clusterObjToUpdate struct {
	clusterObj           *api.Cluster
	rootCARoots          []byte
	rootCASigningCert    []byte
	rootCASigningKey     []byte
	rootCAIntermediates  []byte
	externalCertSignedBy []byte
}

func TestCAServerUpdateRootCA(t *testing.T) {
	// this one needs both external CA servers for testing
	if !cautils.External {
		return
	}

	fakeClusterSpec := func(rootCerts, key []byte, rotation *api.RootRotation, externalCAs []*api.ExternalCA) *api.Cluster {
		return &api.Cluster{
			RootCA: api.RootCA{
				CACert:     rootCerts,
				CAKey:      key,
				CACertHash: "hash",
				JoinTokens: api.JoinTokens{
					Worker:  "SWMTKN-1-worker",
					Manager: "SWMTKN-1-manager",
				},
				RootRotation: rotation,
			},
			Spec: api.ClusterSpec{
				CAConfig: api.CAConfig{
					ExternalCAs: externalCAs,
				},
			},
		}
	}

	tc := cautils.NewTestCA(t)
	require.NoError(t, tc.CAServer.Stop())
	defer tc.Stop()

	cert, key, err := cautils.CreateRootCertAndKey("new root to rotate to")
	require.NoError(t, err)
	newRootCA, err := ca.NewRootCA(append(tc.RootCA.Certs, cert...), cert, key, ca.DefaultNodeCertExpiration, nil)
	require.NoError(t, err)
	externalServer, err := cautils.NewExternalSigningServer(newRootCA, tc.TempDir)
	require.NoError(t, err)
	defer externalServer.Stop()
	crossSigned, err := tc.RootCA.CrossSignCACertificate(cert)
	require.NoError(t, err)

	for i, testCase := range []clusterObjToUpdate{
		{
			clusterObj: fakeClusterSpec(tc.RootCA.Certs, nil, nil, []*api.ExternalCA{{
				Protocol: api.ExternalCA_CAProtocolCFSSL,
				URL:      tc.ExternalSigningServer.URL,
				// without a CA cert, the URL gets successfully added, and there should be no error connecting to it
			}}),
			rootCARoots:          tc.RootCA.Certs,
			externalCertSignedBy: tc.RootCA.Certs,
		},
		{
			clusterObj: fakeClusterSpec(tc.RootCA.Certs, nil, &api.RootRotation{
				CACert:            cert,
				CAKey:             key,
				CrossSignedCACert: crossSigned,
			}, []*api.ExternalCA{
				{
					Protocol: api.ExternalCA_CAProtocolCFSSL,
					URL:      tc.ExternalSigningServer.URL,
					// without a CA cert, we count this as the old tc.RootCA.Certs, and this should be ignored because we want the new root
				},
			}),
			rootCARoots:         tc.RootCA.Certs,
			rootCASigningCert:   crossSigned,
			rootCASigningKey:    key,
			rootCAIntermediates: crossSigned,
		},
		{
			clusterObj: fakeClusterSpec(tc.RootCA.Certs, nil, &api.RootRotation{
				CACert:            cert,
				CrossSignedCACert: crossSigned,
			}, []*api.ExternalCA{
				{
					Protocol: api.ExternalCA_CAProtocolCFSSL,
					URL:      tc.ExternalSigningServer.URL,
					// without a CA cert, we count this as the old tc.RootCA.Certs
				},
				{
					Protocol: api.ExternalCA_CAProtocolCFSSL,
					URL:      externalServer.URL,
					CACert:   append(cert, '\n'),
				},
			}),
			rootCARoots:          tc.RootCA.Certs,
			rootCAIntermediates:  crossSigned,
			externalCertSignedBy: cert,
		},
	} {
		require.NoError(t, tc.CAServer.UpdateRootCA(context.Background(), testCase.clusterObj))

		rootCA := tc.ServingSecurityConfig.RootCA()
		require.Equal(t, testCase.rootCARoots, rootCA.Certs)
		var signingCert, signingKey []byte
		if s, err := rootCA.Signer(); err == nil {
			signingCert, signingKey = s.Cert, s.Key
		}
		require.Equal(t, testCase.rootCARoots, rootCA.Certs)
		require.Equal(t, testCase.rootCASigningCert, signingCert, "%d", i)
		require.Equal(t, testCase.rootCASigningKey, signingKey, "%d", i)
		require.Equal(t, testCase.rootCAIntermediates, rootCA.Intermediates)

		externalCA := tc.ServingSecurityConfig.ExternalCA()
		csr, _, err := ca.GenerateNewCSR()
		require.NoError(t, err)
		signedCert, err := externalCA.Sign(context.Background(), ca.PrepareCSR(csr, "cn", ca.ManagerRole, tc.Organization))

		if testCase.externalCertSignedBy != nil {
			require.NoError(t, err)
			parsed, err := helpers.ParseCertificatePEM(signedCert)
			require.NoError(t, err)
			rootPool := x509.NewCertPool()
			rootPool.AppendCertsFromPEM(testCase.externalCertSignedBy)
			_, err = parsed.Verify(x509.VerifyOptions{Roots: rootPool})
			require.NoError(t, err)
		} else {
			require.Equal(t, ca.ErrNoExternalCAURLs, err)
		}
	}

	// If we can't save the root cert, we can't update the root CA even if it's completely valid
	require.NoError(t, os.RemoveAll(tc.TempDir))
	require.NoError(t, ioutil.WriteFile(tc.TempDir, []byte("cant create directory if this is file"), 0700))
	tc.CAServer.UpdateRootCA(context.Background(), fakeClusterSpec(cautils.ECDSA256SHA256Cert, cautils.ECDSA256Key, nil, nil))
	require.Equal(t, tc.RootCA.Certs, tc.ServingSecurityConfig.RootCA().Certs)
}

type rootRotationTester struct {
	tc *cautils.TestCA
	t  *testing.T
}

// go through all the nodes and update/create the ones we want, and delete the ones
// we don't
func (r *rootRotationTester) convergeWantedNodes(wantNodes map[string]*api.Node, descr string) {
	// update existing and create new nodes first before deleting nodes, else a root rotation
	// may finish early if all the nodes get deleted when the root rotation happens
	require.NoError(r.t, r.tc.MemoryStore.Update(func(tx store.Tx) error {
		for nodeID, wanted := range wantNodes {
			node := store.GetNode(tx, nodeID)
			if node == nil {
				if err := store.CreateNode(tx, wanted); err != nil {
					return err
				}
				continue
			}
			node.Description = wanted.Description
			node.Certificate = wanted.Certificate
			if err := store.UpdateNode(tx, node); err != nil {
				return err
			}
		}
		nodes, err := store.FindNodes(tx, store.All)
		if err != nil {
			return err
		}
		for _, node := range nodes {
			if _, inWanted := wantNodes[node.ID]; !inWanted {
				if err := store.DeleteNode(tx, node.ID); err != nil {
					return err
				}
			}
		}
		return nil
	}), descr)
}

func (r *rootRotationTester) convergeRootCA(wantRootCA *api.RootCA, descr string) {
	require.NoError(r.t, r.tc.MemoryStore.Update(func(tx store.Tx) error {
		clusters, err := store.FindClusters(tx, store.All)
		if err != nil || len(clusters) != 1 {
			return errors.Wrap(err, "unable to find cluster")
		}
		clusters[0].RootCA = *wantRootCA
		return store.UpdateCluster(tx, clusters[0])
	}), descr)
}

func getFakeAPINode(t *testing.T, id string, state api.IssuanceStatus_State, tlsInfo *api.NodeTLSInfo, member bool) *api.Node {
	node := &api.Node{
		ID: id,
		Certificate: api.Certificate{
			Status: api.IssuanceStatus{
				State: state,
			},
		},
		Spec: api.NodeSpec{
			Membership: api.NodeMembershipAccepted,
		},
	}
	if !member {
		node.Spec.Membership = api.NodeMembershipPending
	}
	// the CA server will immediately pick these up, so generate CSRs for the CA server to sign
	if state == api.IssuanceStateRenew || state == api.IssuanceStatePending {
		csr, _, err := ca.GenerateNewCSR()
		require.NoError(t, err)
		node.Certificate.CSR = csr
	}
	if tlsInfo != nil {
		node.Description = &api.NodeDescription{TLSInfo: tlsInfo}
	}
	return node
}

func startCAServer(caServer *ca.Server) {
	alreadyRunning := make(chan struct{})
	go func() {
		if err := caServer.Run(context.Background()); err != nil {
			close(alreadyRunning)
		}
	}()
	select {
	case <-caServer.Ready():
	case <-alreadyRunning:
	}
}

func getRotationInfo(t *testing.T, rotationCert []byte, rootCA *ca.RootCA) ([]byte, *api.NodeTLSInfo) {
	parsedNewRoot, err := helpers.ParseCertificatePEM(rotationCert)
	require.NoError(t, err)
	crossSigned, err := rootCA.CrossSignCACertificate(rotationCert)
	require.NoError(t, err)
	return crossSigned, &api.NodeTLSInfo{
		TrustRoot:           rootCA.Certs,
		CertIssuerPublicKey: parsedNewRoot.RawSubjectPublicKeyInfo,
		CertIssuerSubject:   parsedNewRoot.RawSubject,
	}
}

// These are the root rotation test cases where we expect there to be a change in the FindNodes
// or root CA values after converging.
func TestRootRotationReconciliationWithChanges(t *testing.T) {
	t.Parallel()
	if cautils.External {
		// the external CA functionality is unrelated to testing the reconciliation loop
		return
	}

	tc := cautils.NewTestCA(t)
	defer tc.Stop()
	rt := rootRotationTester{
		tc: tc,
		t:  t,
	}

	rotationCerts := [][]byte{cautils.ECDSA256SHA256Cert, cautils.ECDSACertChain[2]}
	rotationKeys := [][]byte{cautils.ECDSA256Key, cautils.ECDSACertChainKeys[2]}
	var (
		rotationCrossSigned [][]byte
		rotationTLSInfo     []*api.NodeTLSInfo
	)
	for _, cert := range rotationCerts {
		cross, info := getRotationInfo(t, cert, &tc.RootCA)
		rotationCrossSigned = append(rotationCrossSigned, cross)
		rotationTLSInfo = append(rotationTLSInfo, info)
	}

	oldNodeTLSInfo := &api.NodeTLSInfo{
		TrustRoot:           tc.RootCA.Certs,
		CertIssuerPublicKey: tc.ServingSecurityConfig.IssuerInfo().PublicKey,
		CertIssuerSubject:   tc.ServingSecurityConfig.IssuerInfo().Subject,
	}

	var startCluster *api.Cluster
	tc.MemoryStore.View(func(tx store.ReadTx) {
		startCluster = store.GetCluster(tx, tc.Organization)
	})
	require.NotNil(t, startCluster)

	testcases := []struct {
		nodes           map[string]*api.Node // what nodes we should start with
		rootCA          *api.RootCA          // what root CA we should start with
		expectedNodes   map[string]*api.Node // what nodes we expect in the end, if nil, then unchanged from the start
		expectedRootCA  *api.RootCA          // what root CA we expect in the end, if nil, then unchanged from the start
		caServerRestart bool                 // whether to stop the CA server before making the node and root changes and restart after
		descr           string
	}{
		{
			descr: ("If there is no TLS info, the reconciliation cycle tells the nodes to rotate if they're not already getting " +
				"a new cert.  Any renew/pending nodes will have certs issued, but because the TLS info is nil, they will " +
				`go "rotate" state`),
			nodes: map[string]*api.Node{
				"0": getFakeAPINode(t, "0", api.IssuanceStatePending, nil, false),
				"1": getFakeAPINode(t, "1", api.IssuanceStateIssued, nil, true),
				"2": getFakeAPINode(t, "2", api.IssuanceStateRenew, nil, true),
				"3": getFakeAPINode(t, "3", api.IssuanceStateRotate, nil, true),
				"4": getFakeAPINode(t, "4", api.IssuanceStatePending, nil, true),
				"5": getFakeAPINode(t, "5", api.IssuanceStateFailed, nil, true),
			},
			rootCA: &api.RootCA{
				CACert:     startCluster.RootCA.CACert,
				CAKey:      startCluster.RootCA.CAKey,
				CACertHash: startCluster.RootCA.CACertHash,
				RootRotation: &api.RootRotation{
					CACert:            rotationCerts[0],
					CAKey:             rotationKeys[0],
					CrossSignedCACert: rotationCrossSigned[0],
				},
			},
			expectedNodes: map[string]*api.Node{
				"0": getFakeAPINode(t, "0", api.IssuanceStatePending, nil, false),
				"1": getFakeAPINode(t, "1", api.IssuanceStateRotate, nil, true),
				"2": getFakeAPINode(t, "2", api.IssuanceStateRotate, nil, true),
				"3": getFakeAPINode(t, "3", api.IssuanceStateRotate, nil, true),
				"4": getFakeAPINode(t, "4", api.IssuanceStateRotate, nil, true),
				"5": getFakeAPINode(t, "5", api.IssuanceStateRotate, nil, true),
			},
		},
		{
			descr: ("Assume all of the nodes have gotten certs, but some of them are the wrong cert " +
				"(going by the TLS info), which shouldn't really happen.  the rotation reconciliation " +
				"will tell the wrong ones to rotate a second time"),
			nodes: map[string]*api.Node{
				"0": getFakeAPINode(t, "0", api.IssuanceStatePending, nil, false),
				"1": getFakeAPINode(t, "1", api.IssuanceStateIssued, rotationTLSInfo[0], true),
				"2": getFakeAPINode(t, "2", api.IssuanceStateIssued, oldNodeTLSInfo, true),
				"3": getFakeAPINode(t, "3", api.IssuanceStateIssued, rotationTLSInfo[0], true),
				"4": getFakeAPINode(t, "4", api.IssuanceStateIssued, rotationTLSInfo[0], true),
				"5": getFakeAPINode(t, "5", api.IssuanceStateIssued, oldNodeTLSInfo, true),
			},
			rootCA: &api.RootCA{ // no change in root CA from previous
				CACert:     startCluster.RootCA.CACert,
				CAKey:      startCluster.RootCA.CAKey,
				CACertHash: startCluster.RootCA.CACertHash,
				RootRotation: &api.RootRotation{
					CACert:            rotationCerts[0],
					CAKey:             rotationKeys[0],
					CrossSignedCACert: rotationCrossSigned[0],
				},
			},
			expectedNodes: map[string]*api.Node{
				"0": getFakeAPINode(t, "0", api.IssuanceStatePending, nil, false),
				"1": getFakeAPINode(t, "1", api.IssuanceStateIssued, rotationTLSInfo[0], true),
				"2": getFakeAPINode(t, "2", api.IssuanceStateRotate, oldNodeTLSInfo, true),
				"3": getFakeAPINode(t, "3", api.IssuanceStateIssued, rotationTLSInfo[0], true),
				"4": getFakeAPINode(t, "4", api.IssuanceStateIssued, rotationTLSInfo[0], true),
				"5": getFakeAPINode(t, "5", api.IssuanceStateRotate, oldNodeTLSInfo, true),
			},
		},
		{
			descr: ("New nodes that are added will also be picked up and told to rotate"),
			nodes: map[string]*api.Node{
				"0": getFakeAPINode(t, "0", api.IssuanceStatePending, nil, false),
				"1": getFakeAPINode(t, "1", api.IssuanceStateIssued, rotationTLSInfo[0], true),
				"3": getFakeAPINode(t, "3", api.IssuanceStateIssued, rotationTLSInfo[0], true),
				"4": getFakeAPINode(t, "4", api.IssuanceStateIssued, rotationTLSInfo[0], true),
				"5": getFakeAPINode(t, "5", api.IssuanceStateIssued, rotationTLSInfo[0], true),
				"6": getFakeAPINode(t, "6", api.IssuanceStateRenew, nil, true),
			},
			rootCA: &api.RootCA{ // no change in root CA from previous
				CACert:     startCluster.RootCA.CACert,
				CAKey:      startCluster.RootCA.CAKey,
				CACertHash: startCluster.RootCA.CACertHash,
				RootRotation: &api.RootRotation{
					CACert:            rotationCerts[0],
					CAKey:             rotationKeys[0],
					CrossSignedCACert: rotationCrossSigned[0],
				},
			},
			expectedNodes: map[string]*api.Node{
				"0": getFakeAPINode(t, "0", api.IssuanceStatePending, nil, false),
				"1": getFakeAPINode(t, "1", api.IssuanceStateIssued, rotationTLSInfo[0], true),
				"3": getFakeAPINode(t, "3", api.IssuanceStateIssued, rotationTLSInfo[0], true),
				"4": getFakeAPINode(t, "4", api.IssuanceStateIssued, rotationTLSInfo[0], true),
				"5": getFakeAPINode(t, "5", api.IssuanceStateIssued, rotationTLSInfo[0], true),
				"6": getFakeAPINode(t, "6", api.IssuanceStateRotate, nil, true),
			},
		},
		{
			descr: ("Even if root rotation isn't finished, if the root changes again to a " +
				"different cert, all the nodes with the old root rotation cert will be told " +
				"to rotate again."),
			nodes: map[string]*api.Node{
				"0": getFakeAPINode(t, "0", api.IssuanceStatePending, nil, false),
				"1": getFakeAPINode(t, "1", api.IssuanceStateIssued, rotationTLSInfo[0], true),
				"3": getFakeAPINode(t, "3", api.IssuanceStateIssued, rotationTLSInfo[1], true),
				"4": getFakeAPINode(t, "4", api.IssuanceStateIssued, rotationTLSInfo[0], true),
				"5": getFakeAPINode(t, "5", api.IssuanceStateIssued, oldNodeTLSInfo, true),
				"6": getFakeAPINode(t, "6", api.IssuanceStateIssued, rotationTLSInfo[0], true),
			},
			rootCA: &api.RootCA{ // new root rotation
				CACert:     startCluster.RootCA.CACert,
				CAKey:      startCluster.RootCA.CAKey,
				CACertHash: startCluster.RootCA.CACertHash,
				RootRotation: &api.RootRotation{
					CACert:            rotationCerts[1],
					CAKey:             rotationKeys[1],
					CrossSignedCACert: rotationCrossSigned[1],
				},
			},
			expectedNodes: map[string]*api.Node{
				"0": getFakeAPINode(t, "0", api.IssuanceStatePending, nil, false),
				"1": getFakeAPINode(t, "1", api.IssuanceStateRotate, rotationTLSInfo[0], true),
				"3": getFakeAPINode(t, "3", api.IssuanceStateIssued, rotationTLSInfo[1], true),
				"4": getFakeAPINode(t, "4", api.IssuanceStateRotate, rotationTLSInfo[0], true),
				"5": getFakeAPINode(t, "5", api.IssuanceStateRotate, oldNodeTLSInfo, true),
				"6": getFakeAPINode(t, "6", api.IssuanceStateRotate, rotationTLSInfo[0], true),
			},
		},
		{
			descr: ("Once all nodes have rotated to their desired TLS info (even if it's because " +
				"a node with the wrong TLS info has been removed, the root rotation is completed."),
			nodes: map[string]*api.Node{
				"0": getFakeAPINode(t, "0", api.IssuanceStatePending, nil, false),
				"1": getFakeAPINode(t, "1", api.IssuanceStateIssued, rotationTLSInfo[1], true),
				"3": getFakeAPINode(t, "3", api.IssuanceStateIssued, rotationTLSInfo[1], true),
				"4": getFakeAPINode(t, "4", api.IssuanceStateIssued, rotationTLSInfo[1], true),
				"6": getFakeAPINode(t, "6", api.IssuanceStateIssued, rotationTLSInfo[1], true),
			},
			rootCA: &api.RootCA{
				// no change in root CA from previous - even if root rotation gets completed after
				// the nodes are first set, and we just add the root rotation again because of this
				// test order, because the TLS info is correct for all nodes it will be completed again
				// anyway)
				CACert:     startCluster.RootCA.CACert,
				CAKey:      startCluster.RootCA.CAKey,
				CACertHash: startCluster.RootCA.CACertHash,
				RootRotation: &api.RootRotation{
					CACert:            rotationCerts[1],
					CAKey:             rotationKeys[1],
					CrossSignedCACert: rotationCrossSigned[1],
				},
			},
			expectedRootCA: &api.RootCA{
				CACert:     rotationCerts[1],
				CAKey:      rotationKeys[1],
				CACertHash: digest.FromBytes(rotationCerts[1]).String(),
				// ignore the join tokens - we aren't comparing them
			},
		},
		{
			descr: ("If a root rotation happens when the CA server is down, so long as it saw the change " +
				"it will start reconciling the nodes as soon as it's started up again"),
			caServerRestart: true,
			nodes: map[string]*api.Node{
				"0": getFakeAPINode(t, "0", api.IssuanceStatePending, nil, false),
				"1": getFakeAPINode(t, "1", api.IssuanceStateIssued, rotationTLSInfo[1], true),
				"3": getFakeAPINode(t, "3", api.IssuanceStateIssued, rotationTLSInfo[1], true),
				"4": getFakeAPINode(t, "4", api.IssuanceStateIssued, rotationTLSInfo[1], true),
				"6": getFakeAPINode(t, "6", api.IssuanceStateIssued, rotationTLSInfo[1], true),
			},
			rootCA: &api.RootCA{
				CACert:     startCluster.RootCA.CACert,
				CAKey:      startCluster.RootCA.CAKey,
				CACertHash: startCluster.RootCA.CACertHash,
				RootRotation: &api.RootRotation{
					CACert:            rotationCerts[0],
					CAKey:             rotationKeys[0],
					CrossSignedCACert: rotationCrossSigned[0],
				},
			},
			expectedNodes: map[string]*api.Node{
				"0": getFakeAPINode(t, "0", api.IssuanceStatePending, nil, false),
				"1": getFakeAPINode(t, "1", api.IssuanceStateRotate, rotationTLSInfo[1], true),
				"3": getFakeAPINode(t, "3", api.IssuanceStateRotate, rotationTLSInfo[1], true),
				"4": getFakeAPINode(t, "4", api.IssuanceStateRotate, rotationTLSInfo[1], true),
				"6": getFakeAPINode(t, "6", api.IssuanceStateRotate, rotationTLSInfo[1], true),
			},
		},
	}

	for _, testcase := range testcases {
		if testcase.caServerRestart {
			rt.tc.CAServer.Stop()
		}

		rt.convergeRootCA(testcase.rootCA, testcase.descr)
		rt.convergeWantedNodes(testcase.nodes, testcase.descr)

		if testcase.expectedNodes == nil {
			testcase.expectedNodes = testcase.nodes
		}
		if testcase.expectedRootCA == nil {
			testcase.expectedRootCA = testcase.rootCA
		}

		if testcase.caServerRestart {
			startCAServer(rt.tc.CAServer)
		}

		require.NoError(t, testutils.PollFuncWithTimeout(nil, func() error {
			var (
				nodes   []*api.Node
				cluster *api.Cluster
				err     error
			)
			tc.MemoryStore.View(func(tx store.ReadTx) {
				nodes, err = store.FindNodes(tx, store.All)
				cluster = store.GetCluster(tx, tc.Organization)
			})
			if err != nil {
				return err
			}
			if cluster == nil {
				return errors.New("no cluster found")
			}

			if !equality.RootCAEqualStable(&cluster.RootCA, testcase.expectedRootCA) {
				return fmt.Errorf("root CAs not equal:\n\texpected: %v\n\tactual: %v", *testcase.expectedRootCA, cluster.RootCA)
			}
			if len(nodes) != len(testcase.expectedNodes) {
				return fmt.Errorf("number of expected nodes (%d) does not equal number of actual nodes (%d)",
					len(testcase.expectedNodes), len(nodes))
			}
			for _, node := range nodes {
				expected, ok := testcase.expectedNodes[node.ID]
				if !ok {
					return fmt.Errorf("node %s is present and was unexpected", node.ID)
				}
				if !reflect.DeepEqual(expected.Description, node.Description) {
					return fmt.Errorf("the node description of node %s is not expected:\n\texpected: %v\n\tactual: %v", node.ID,
						expected.Description, node.Description)
				}
				if !reflect.DeepEqual(expected.Certificate.Status, node.Certificate.Status) {
					return fmt.Errorf("the certificate status of node %s is not expected:\n\texpected: %v\n\tactual: %v", node.ID,
						expected.Certificate, node.Certificate)
				}

				// ensure that the security config's root CA object has the same expected key
				expectedKey := testcase.expectedRootCA.CAKey
				if testcase.expectedRootCA.RootRotation != nil {
					expectedKey = testcase.expectedRootCA.RootRotation.CAKey
				}
				s, err := rt.tc.ServingSecurityConfig.RootCA().Signer()
				if err != nil {
					return err
				}
				if !bytes.Equal(s.Key, expectedKey) {
					return fmt.Errorf("the security config has not been updated correctly")
				}
			}
			return nil
		}, 5*time.Second), testcase.descr)
	}
}

// These are the root rotation test cases where we expect there to be no changes made to either
// the nodes or the root CA object
func TestRootRotationReconciliationNoChanges(t *testing.T) {
	t.Parallel()
	if cautils.External {
		// the external CA functionality is unrelated to testing the reconciliation loop
		return
	}

	tc := cautils.NewTestCA(t)
	defer tc.Stop()
	rt := rootRotationTester{
		tc: tc,
		t:  t,
	}

	rotationCert := cautils.ECDSA256SHA256Cert
	rotationKey := cautils.ECDSA256Key
	rotationCrossSigned, rotationTLSInfo := getRotationInfo(t, rotationCert, &tc.RootCA)

	oldNodeTLSInfo := &api.NodeTLSInfo{
		TrustRoot:           tc.RootCA.Certs,
		CertIssuerPublicKey: tc.ServingSecurityConfig.IssuerInfo().PublicKey,
		CertIssuerSubject:   tc.ServingSecurityConfig.IssuerInfo().Subject,
	}

	var startCluster *api.Cluster
	tc.MemoryStore.View(func(tx store.ReadTx) {
		startCluster = store.GetCluster(tx, tc.Organization)
	})
	require.NotNil(t, startCluster)

	testcases := []struct {
		nodes           map[string]*api.Node // what nodes we should start with
		rootCA          *api.RootCA          // what root CA we should start with
		descr           string
		caServerStopped bool // if the server is running, only then will a reconciliation loop happen
	}{
		{
			descr: ("If the CA server is not running no reconciliation happens even if a root rotation " +
				"is in progress"),
			caServerStopped: true,
			nodes: map[string]*api.Node{
				"0": getFakeAPINode(t, "0", api.IssuanceStatePending, nil, false),
				"1": getFakeAPINode(t, "1", api.IssuanceStateIssued, oldNodeTLSInfo, true),
				"2": getFakeAPINode(t, "2", api.IssuanceStateRenew, nil, true),
				"3": getFakeAPINode(t, "3", api.IssuanceStateRotate, nil, true),
				"4": getFakeAPINode(t, "4", api.IssuanceStatePending, nil, true),
				"5": getFakeAPINode(t, "5", api.IssuanceStateFailed, nil, true),
			},
			rootCA: &api.RootCA{
				CACert:     startCluster.RootCA.CACert,
				CAKey:      startCluster.RootCA.CAKey,
				CACertHash: startCluster.RootCA.CACertHash,
				RootRotation: &api.RootRotation{
					CACert:            rotationCert,
					CAKey:             rotationKey,
					CrossSignedCACert: rotationCrossSigned,
				},
			},
		},
		{
			descr: ("If all nodes have the right TLS info or are already rotated (or are not members), " +
				"there will be no changes needed"),
			nodes: map[string]*api.Node{
				"0": getFakeAPINode(t, "0", api.IssuanceStatePending, nil, false),
				"1": getFakeAPINode(t, "1", api.IssuanceStateIssued, rotationTLSInfo, true),
				"2": getFakeAPINode(t, "2", api.IssuanceStateRotate, oldNodeTLSInfo, true),
				"3": getFakeAPINode(t, "3", api.IssuanceStateRotate, rotationTLSInfo, true),
			},
			rootCA: &api.RootCA{ // no change in root CA from previous
				CACert:     startCluster.RootCA.CACert,
				CAKey:      startCluster.RootCA.CAKey,
				CACertHash: startCluster.RootCA.CACertHash,
				RootRotation: &api.RootRotation{
					CACert:            rotationCert,
					CAKey:             rotationKey,
					CrossSignedCACert: rotationCrossSigned,
				},
			},
		},
		{
			descr: ("Nodes already in rotate state, even if they currently have the correct TLS issuer, will be " +
				"left in the rotate state even if root rotation is aborted because we don't know if they're already " +
				"in the process of getting a new cert.  Even if they're issued by a different issuer, they will be " +
				"left alone because they'll have an interemdiate that chains up to the old issuer."),
			nodes: map[string]*api.Node{
				"0": getFakeAPINode(t, "0", api.IssuanceStatePending, nil, false),
				"1": getFakeAPINode(t, "1", api.IssuanceStateIssued, rotationTLSInfo, true),
				"2": getFakeAPINode(t, "2", api.IssuanceStateRotate, oldNodeTLSInfo, true),
			},
			rootCA: &api.RootCA{ // no change in root CA from previous
				CACert:     startCluster.RootCA.CACert,
				CAKey:      startCluster.RootCA.CAKey,
				CACertHash: startCluster.RootCA.CACertHash,
			},
		},
	}

	for _, testcase := range testcases {
		if testcase.caServerStopped {
			rt.tc.CAServer.Stop()
		} else {
			startCAServer(rt.tc.CAServer)
		}

		rt.convergeRootCA(testcase.rootCA, testcase.descr)
		rt.convergeWantedNodes(testcase.nodes, testcase.descr)

		time.Sleep(500 * time.Millisecond)

		var (
			nodes   []*api.Node
			cluster *api.Cluster
			err     error
		)

		tc.MemoryStore.View(func(tx store.ReadTx) {
			nodes, err = store.FindNodes(tx, store.All)
			cluster = store.GetCluster(tx, tc.Organization)
		})
		require.NoError(t, err)
		require.NotNil(t, cluster)
		require.Equal(t, cluster.RootCA, *testcase.rootCA, testcase.descr)
		require.Len(t, nodes, len(testcase.nodes), testcase.descr)
		for _, node := range nodes {
			expected, ok := testcase.nodes[node.ID]
			require.True(t, ok, "node %s: %s", node.ID, testcase.descr)
			require.Equal(t, expected.Description, node.Description, "node %s: %s", node.ID, testcase.descr)
			require.Equal(t, expected.Certificate.Status, node.Certificate.Status, "node %s: %s", node.ID, testcase.descr)
		}

		// ensure that the security config's root CA object has the same expected key
		expectedKey := testcase.rootCA.CAKey
		if testcase.rootCA.RootRotation != nil {
			expectedKey = testcase.rootCA.RootRotation.CAKey
		}
		s, err := rt.tc.ServingSecurityConfig.RootCA().Signer()
		require.NoError(t, err, testcase.descr)
		require.Equal(t, s.Key, expectedKey, testcase.descr)
	}
}

// Tests if the root rotation changes while the reconciliation loop is going, eventually the root rotation will finish
// successfully (even if there's a competing reconciliation loop, for instance if there's a bug during leadership handoff).
func TestRootRotationReconciliationRace(t *testing.T) {
	t.Parallel()
	if cautils.External {
		// the external CA functionality is unrelated to testing the reconciliation loop
		return
	}

	tc := cautils.NewTestCA(t)
	defer tc.Stop()
	rt := rootRotationTester{
		tc: tc,
		t:  t,
	}

	tempDir, err := ioutil.TempDir("", "competing-ca-server")
	require.NoError(t, err)
	defer os.RemoveAll(tempDir)

	var otherServers []*ca.Server
	var secConfigs []*ca.SecurityConfig
	for i := 0; i < 3; i++ { // to make sure we get some collision
		// start a competing CA server
		competingSecConfig, err := tc.NewNodeConfig(ca.ManagerRole)
		require.NoError(t, err)
		secConfigs = append(secConfigs, competingSecConfig)

		paths := ca.NewConfigPaths(filepath.Join(tempDir, fmt.Sprintf("%d", i)))

		otherServer := ca.NewServer(tc.MemoryStore, competingSecConfig, paths.RootCA)
		// offset each server's reconciliation interval somewhat so that some will
		// pre-empt others
		otherServer.SetRootReconciliationInterval(time.Millisecond * time.Duration((i+1)*10))
		startCAServer(otherServer)
		defer otherServer.Stop()
		otherServers = append(otherServers, otherServer)
	}
	clusterWatch, clusterWatchCancel, err := store.ViewAndWatch(
		tc.MemoryStore, func(tx store.ReadTx) error {
			// don't bother getting the cluster - the CA serverß have already done that when first running
			return nil
		},
		api.EventUpdateCluster{
			Cluster: &api.Cluster{ID: tc.Organization},
			Checks:  []api.ClusterCheckFunc{api.ClusterCheckID},
		},
	)
	require.NoError(t, err)
	defer clusterWatchCancel()

	done := make(chan struct{})
	defer close(done)
	go func() {
		for {
			select {
			case event := <-clusterWatch:
				clusterEvent := event.(api.EventUpdateCluster)
				for _, s := range otherServers {
					s.UpdateRootCA(context.Background(), clusterEvent.Cluster)
				}
			case <-done:
				return
			}
		}
	}()

	oldNodeTLSInfo := &api.NodeTLSInfo{
		TrustRoot:           tc.RootCA.Certs,
		CertIssuerPublicKey: tc.ServingSecurityConfig.IssuerInfo().PublicKey,
		CertIssuerSubject:   tc.ServingSecurityConfig.IssuerInfo().Subject,
	}

	nodes := make(map[string]*api.Node)
	for i := 0; i < 5; i++ {
		nodeID := fmt.Sprintf("%d", i)
		nodes[nodeID] = getFakeAPINode(t, nodeID, api.IssuanceStateIssued, oldNodeTLSInfo, true)
	}
	rt.convergeWantedNodes(nodes, "setting up nodes for root rotation race condition test")

	var rotationCert, rotationKey []byte
	for i := 0; i < 10; i++ {
		var (
			rotationCrossSigned []byte
			rotationTLSInfo     *api.NodeTLSInfo
		)
		rotationCert, rotationKey, err = cautils.CreateRootCertAndKey(fmt.Sprintf("root cn %d", i))
		require.NoError(t, err)
		require.NoError(t, tc.MemoryStore.Update(func(tx store.Tx) error {
			cluster := store.GetCluster(tx, tc.Organization)
			if cluster == nil {
				return errors.New("cluster has disappeared")
			}
			rootCA := cluster.RootCA.Copy()
			caRootCA, err := ca.NewRootCA(rootCA.CACert, rootCA.CACert, rootCA.CAKey, ca.DefaultNodeCertExpiration, nil)
			if err != nil {
				return err
			}
			rotationCrossSigned, rotationTLSInfo = getRotationInfo(t, rotationCert, &caRootCA)
			rootCA.RootRotation = &api.RootRotation{
				CACert:            rotationCert,
				CAKey:             rotationKey,
				CrossSignedCACert: rotationCrossSigned,
			}
			cluster.RootCA = *rootCA
			return store.UpdateCluster(tx, cluster)
		}))
		for _, node := range nodes {
			node.Description.TLSInfo = rotationTLSInfo
		}
		rt.convergeWantedNodes(nodes, fmt.Sprintf("iteration %d", i))
	}

	require.NoError(t, testutils.PollFuncWithTimeout(nil, func() error {
		var cluster *api.Cluster
		tc.MemoryStore.View(func(tx store.ReadTx) {
			cluster = store.GetCluster(tx, tc.Organization)
		})
		if cluster == nil {
			return errors.New("cluster has disappeared")
		}
		if cluster.RootCA.RootRotation != nil {
			return errors.New("root rotation is still present")
		}
		if !bytes.Equal(cluster.RootCA.CACert, rotationCert) {
			return errors.New("expected root cert is wrong")
		}
		if !bytes.Equal(cluster.RootCA.CAKey, rotationKey) {
			return errors.New("expected root key is wrong")
		}
		for _, secConfig := range secConfigs {
			s, err := secConfig.RootCA().Signer()
			if err != nil {
				return err
			}
			if !bytes.Equal(s.Key, rotationKey) {
				return errors.New("all the sec configs haven't been updated yet")
			}
		}
		return nil
	}, 5*time.Second))

	// all of the ca servers have the appropriate cert and key
}

// If there are a lot of nodes, we only update a small number of them at once.
func TestRootRotationReconciliationThrottled(t *testing.T) {
	t.Parallel()
	if cautils.External {
		// the external CA functionality is unrelated to testing the reconciliation loop
		return
	}

	tc := cautils.NewTestCA(t)
	defer tc.Stop()
	// immediately stop the CA server - we want to run our down
	tc.CAServer.Stop()

	caServer := ca.NewServer(tc.MemoryStore, tc.ServingSecurityConfig, tc.Paths.RootCA)
	// set the reconciliation interval to something ridiculous, so we can make sure the first
	// batch does update all of them
	caServer.SetRootReconciliationInterval(time.Hour)
	startCAServer(caServer)
	defer caServer.Stop()

	var nodes []*api.Node
	clusterWatch, clusterWatchCancel, err := store.ViewAndWatch(
		tc.MemoryStore, func(tx store.ReadTx) error {
			// don't bother getting the cluster - the CA server has already done that when first running
			var err error
			nodes, err = store.FindNodes(tx, store.ByMembership(api.NodeMembershipAccepted))
			return err
		},
		api.EventUpdateCluster{
			Cluster: &api.Cluster{ID: tc.Organization},
			Checks:  []api.ClusterCheckFunc{api.ClusterCheckID},
		},
	)
	require.NoError(t, err)
	defer clusterWatchCancel()

	done := make(chan struct{})
	defer close(done)
	go func() {
		for {
			select {
			case event := <-clusterWatch:
				clusterEvent := event.(api.EventUpdateCluster)
				caServer.UpdateRootCA(context.Background(), clusterEvent.Cluster)
			case <-done:
				return
			}
		}
	}()

	// create twice the batch size of nodes
	_, err = tc.MemoryStore.Batch(func(batch *store.Batch) error {
		for i := len(nodes); i < ca.IssuanceStateRotateMaxBatchSize*2; i++ {
			nodeID := fmt.Sprintf("%d", i)
			err := batch.Update(func(tx store.Tx) error {
				return store.CreateNode(tx, getFakeAPINode(t, nodeID, api.IssuanceStateIssued, nil, true))
			})
			if err != nil {
				return err
			}
		}
		return nil
	})
	require.NoError(t, err)

	rotationCert := cautils.ECDSA256SHA256Cert
	rotationKey := cautils.ECDSA256Key
	rotationCrossSigned, _ := getRotationInfo(t, rotationCert, &tc.RootCA)

	require.NoError(t, tc.MemoryStore.Update(func(tx store.Tx) error {
		cluster := store.GetCluster(tx, tc.Organization)
		if cluster == nil {
			return errors.New("cluster has disappeared")
		}
		rootCA := cluster.RootCA.Copy()
		rootCA.RootRotation = &api.RootRotation{
			CACert:            rotationCert,
			CAKey:             rotationKey,
			CrossSignedCACert: rotationCrossSigned,
		}
		cluster.RootCA = *rootCA
		return store.UpdateCluster(tx, cluster)
	}))

	checkRotationNumber := func() error {
		tc.MemoryStore.View(func(tx store.ReadTx) {
			nodes, err = store.FindNodes(tx, store.All)
		})
		var issuanceRotate int
		for _, n := range nodes {
			if n.Certificate.Status.State == api.IssuanceStateRotate {
				issuanceRotate += 1
			}
		}
		if issuanceRotate != ca.IssuanceStateRotateMaxBatchSize {
			return fmt.Errorf("expected %d, got %d", ca.IssuanceStateRotateMaxBatchSize, issuanceRotate)
		}
		return nil
	}

	require.NoError(t, testutils.PollFuncWithTimeout(nil, checkRotationNumber, 5*time.Second))
	// prove that it's not just because the updates haven't finished
	time.Sleep(time.Second)
	require.NoError(t, checkRotationNumber())
}
