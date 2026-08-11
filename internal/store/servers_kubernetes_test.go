package store

import (
	"context"
	"errors"
	"testing"
)

// makeKubernetesServer creates a Kubernetes cluster row. The ServiceAccount
// bearer token rides in Password, the same field a database password uses, so
// it gets the same AAD-bound encryption.
func makeKubernetesServer(t *testing.T, s *Store, key []byte, name string) *Server {
	t.Helper()

	created, err := s.CreateServer(context.Background(), &Server{
		Name:     name,
		Host:     "api.cluster.example.com",
		Port:     6443,
		Username: "dbbat",
		Password: "sa-token-" + name,
		Protocol: ProtocolKubernetes,
		ProtocolData: &ServerProtocolData{
			Kubernetes: &KubernetesServerData{CACert: "-----BEGIN CERTIFICATE-----\nAAAA\n", Namespace: "data"},
		},
	}, key)
	if err != nil {
		t.Fatalf("CreateServer(kubernetes) error = %v", err)
	}

	return created
}

func TestKubernetesServer_TokenAndPublicMaterialRoundTrip(t *testing.T) {
	t.Parallel()

	s := setupTestStore(t)
	ctx := context.Background()
	key := testEncryptionKey()

	created := makeKubernetesServer(t, s, key, "prod-cluster")

	reloaded, err := s.GetServerByUID(ctx, created.UID)
	if err != nil {
		t.Fatalf("GetServerByUID() error = %v", err)
	}

	// The token is a secret: it must be ciphertext at rest.
	if string(reloaded.PasswordEncrypted) == "sa-token-prod-cluster" {
		t.Error("the service account token was stored in clear")
	}
	if err := reloaded.DecryptPassword(key); err != nil {
		t.Fatalf("DecryptPassword() error = %v", err)
	}
	if reloaded.Password != "sa-token-prod-cluster" {
		t.Errorf("decrypted token = %q, want the stored one", reloaded.Password)
	}

	// The CA bundle and namespace are public and stay readable.
	kd := reloaded.KubernetesData()
	if kd == nil {
		t.Fatal("KubernetesData() = nil, want the persisted cluster material")
	}
	if kd.Namespace != "data" {
		t.Errorf("Namespace = %q, want %q", kd.Namespace, "data")
	}
	if kd.CACert == "" {
		t.Error("CACert was not persisted")
	}
}

func TestKubernetesNamespaceOrDefault(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		srv  Server
		want string
	}{
		{"explicit", Server{ProtocolData: &ServerProtocolData{Kubernetes: &KubernetesServerData{Namespace: "data"}}}, "data"},
		{"empty", Server{ProtocolData: &ServerProtocolData{Kubernetes: &KubernetesServerData{}}}, "default"},
		{"absent", Server{}, "default"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			if got := tc.srv.KubernetesNamespaceOrDefault(); got != tc.want {
				t.Errorf("KubernetesNamespaceOrDefault() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestKubernetesServer_IsATunnelNotATarget(t *testing.T) {
	t.Parallel()

	s := setupTestStore(t)
	ctx := context.Background()
	key := testEncryptionKey()

	cluster := makeKubernetesServer(t, s, key, "cluster-scope")

	if !cluster.IsKubernetes() || !cluster.IsTunnel() || cluster.IsSSH() {
		t.Errorf("classification wrong: IsKubernetes=%v IsTunnel=%v IsSSH=%v",
			cluster.IsKubernetes(), cluster.IsTunnel(), cluster.IsSSH())
	}

	// Every targets-only listing must exclude it, exactly like an ssh row.
	targets, err := s.ListServers(ctx)
	if err != nil {
		t.Fatalf("ListServers() error = %v", err)
	}
	for _, srv := range targets {
		if srv.UID == cluster.UID {
			t.Error("ListServers() returned a kubernetes cluster row")
		}
	}

	if _, err := s.GetServerByName(ctx, "cluster-scope"); !errors.Is(err, ErrServerNotFound) {
		t.Errorf("GetServerByName(cluster) error = %v, want ErrServerNotFound", err)
	}

	// ...but the tunnel listing must include it, and the ssh-only one must not.
	tunnels, err := s.ListTunnelServers(ctx)
	if err != nil {
		t.Fatalf("ListTunnelServers() error = %v", err)
	}
	if !containsUID(tunnels, cluster.UID) {
		t.Error("ListTunnelServers() did not return the kubernetes cluster row")
	}

	sshOnly, err := s.ListSSHServers(ctx)
	if err != nil {
		t.Fatalf("ListSSHServers() error = %v", err)
	}
	if containsUID(sshOnly, cluster.UID) {
		t.Error("ListSSHServers() leaked a kubernetes cluster row")
	}
}

func TestValidateViaUID_AcceptsAKubernetesCluster(t *testing.T) {
	t.Parallel()

	s := setupTestStore(t)
	ctx := context.Background()
	key := testEncryptionKey()

	cluster := makeKubernetesServer(t, s, key, "via-cluster")

	target, err := s.CreateServer(ctx, &Server{
		Name: "pg-in-cluster", Host: "svc/postgres", Port: 5432,
		DatabaseName: "app", Username: "app", Password: "pw",
		Protocol: ProtocolPostgreSQL, ViaUID: &cluster.UID,
	}, key)
	if err != nil {
		t.Fatalf("CreateServer(via kubernetes) error = %v", err)
	}
	if target.ViaUID == nil || *target.ViaUID != cluster.UID {
		t.Errorf("ViaUID = %v, want the cluster UID", target.ViaUID)
	}
}

// TestValidateViaUID_AcceptsAClusterBehindABastion covers the supported
// nesting: the API server itself is only reachable through a jump host.
func TestValidateViaUID_AcceptsAClusterBehindABastion(t *testing.T) {
	t.Parallel()

	s := setupTestStore(t)
	ctx := context.Background()
	key := testEncryptionKey()

	bastion := makeSSHServer(t, s, key, "jump", "-----BEGIN OPENSSH PRIVATE KEY-----\nAAAA\n")

	cluster, err := s.CreateServer(ctx, &Server{
		Name: "private-cluster", Host: "api.internal", Port: 6443,
		Username: "dbbat", Password: "sa-token", Protocol: ProtocolKubernetes,
		ViaUID: &bastion.UID,
		ProtocolData: &ServerProtocolData{
			Kubernetes: &KubernetesServerData{Namespace: "data"},
		},
	}, key)
	if err != nil {
		t.Fatalf("CreateServer(cluster via bastion) error = %v", err)
	}

	// A target behind that cluster validates through the whole two-hop chain.
	if _, err := s.CreateServer(ctx, &Server{
		Name: "pg-behind-both", Host: "pg-0", Port: 5432,
		DatabaseName: "app", Username: "app", Password: "pw",
		Protocol: ProtocolPostgreSQL, ViaUID: &cluster.UID,
	}, key); err != nil {
		t.Fatalf("CreateServer(target via cluster via bastion) error = %v", err)
	}
}

func TestValidateViaUID_StillRejectsADatabaseTarget(t *testing.T) {
	t.Parallel()

	s := setupTestStore(t)
	ctx := context.Background()
	key := testEncryptionKey()

	plain, err := s.CreateServer(ctx, &Server{
		Name: "plain-db", Host: "db.example.com", Port: 5432,
		DatabaseName: "app", Username: "app", Password: "pw", Protocol: ProtocolPostgreSQL,
	}, key)
	if err != nil {
		t.Fatalf("CreateServer() error = %v", err)
	}

	_, err = s.CreateServer(ctx, &Server{
		Name: "bad-via", Host: "db2.example.com", Port: 5432,
		DatabaseName: "app", Username: "app", Password: "pw",
		Protocol: ProtocolPostgreSQL, ViaUID: &plain.UID,
	}, key)
	if !errors.Is(err, ErrServerViaNotTunnel) {
		t.Errorf("CreateServer(via a database) error = %v, want ErrServerViaNotTunnel", err)
	}
}

// TestUpdateServer_MergesKubernetesMaterial pins the property that made the
// merge a single Set: PostgreSQL rejects assigning protocol_data twice, so
// editing the CA bundle and the namespace together must still work — and must
// not disturb the sibling protocol_data keys.
func TestUpdateServer_MergesKubernetesMaterial(t *testing.T) {
	t.Parallel()

	s := setupTestStore(t)
	ctx := context.Background()
	key := testEncryptionKey()

	cluster := makeKubernetesServer(t, s, key, "merge-cluster")

	newCA := "-----BEGIN CERTIFICATE-----\nBBBB\n"
	newNS := "staging"
	insecure := true

	if err := s.UpdateServer(ctx, cluster.UID, ServerUpdate{
		K8sCACert:                &newCA,
		K8sNamespace:             &newNS,
		K8sInsecureSkipTLSVerify: &insecure,
	}, key); err != nil {
		t.Fatalf("UpdateServer(ca + namespace + insecure) error = %v", err)
	}

	reloaded, err := s.GetServerByUID(ctx, cluster.UID)
	if err != nil {
		t.Fatalf("GetServerByUID() error = %v", err)
	}

	kd := reloaded.KubernetesData()
	if kd.CACert != newCA || kd.Namespace != newNS || !kd.InsecureSkipTLSVerify {
		t.Errorf("merged data = %+v, want the three updated fields", kd)
	}

	// A partial update must leave the untouched keys alone.
	onlyNS := "prod"
	if err := s.UpdateServer(ctx, cluster.UID, ServerUpdate{K8sNamespace: &onlyNS}, key); err != nil {
		t.Fatalf("UpdateServer(namespace only) error = %v", err)
	}

	reloaded, err = s.GetServerByUID(ctx, cluster.UID)
	if err != nil {
		t.Fatalf("GetServerByUID() error = %v", err)
	}
	if kd = reloaded.KubernetesData(); kd.Namespace != "prod" || kd.CACert != newCA {
		t.Errorf("partial update clobbered a sibling key: %+v", kd)
	}
}

// TestSetKubernetesCACert_MergesTheLearnedPin is the store half of TOFU: the
// learned bundle lands in its own key, leaves the operator-supplied one and
// every sibling key untouched, and can be cleared again.
func TestSetKubernetesCACert_MergesTheLearnedPin(t *testing.T) {
	t.Parallel()

	s := setupTestStore(t)
	ctx := context.Background()
	key := testEncryptionKey()

	cluster := makeKubernetesServer(t, s, key, "tofu-cluster")
	suppliedCA := "-----BEGIN CERTIFICATE-----\nAAAA\n"
	learnedCA := "-----BEGIN CERTIFICATE-----\nLEARNED\n"

	if err := s.SetKubernetesCACert(ctx, cluster.UID, learnedCA); err != nil {
		t.Fatalf("SetKubernetesCACert() error = %v", err)
	}

	reloaded, err := s.GetServerByUID(ctx, cluster.UID)
	if err != nil {
		t.Fatalf("GetServerByUID() error = %v", err)
	}

	kd := reloaded.KubernetesData()
	if kd.LearnedCACert != learnedCA {
		t.Errorf("LearnedCACert = %q, want %q", kd.LearnedCACert, learnedCA)
	}

	if kd.CACert != suppliedCA {
		t.Errorf("CACert = %q, want the operator-supplied bundle untouched", kd.CACert)
	}

	if kd.Namespace != "data" {
		t.Errorf("Namespace = %q, want the sibling key untouched", kd.Namespace)
	}

	// Exit #1: an explicit reset, for a rotated cluster CA you cannot re-paste.
	if err := s.UpdateServer(ctx, cluster.UID, ServerUpdate{K8sClearLearnedCACert: true}, key); err != nil {
		t.Fatalf("UpdateServer(clear learned) error = %v", err)
	}

	reloaded, err = s.GetServerByUID(ctx, cluster.UID)
	if err != nil {
		t.Fatalf("GetServerByUID() error = %v", err)
	}

	if kd = reloaded.KubernetesData(); kd.LearnedCACert != "" {
		t.Errorf("LearnedCACert = %q after a reset, want it forgotten", kd.LearnedCACert)
	}

	// Exit #2: pasting a bundle supersedes the pin, so it must not linger.
	if err := s.SetKubernetesCACert(ctx, cluster.UID, learnedCA); err != nil {
		t.Fatalf("SetKubernetesCACert() error = %v", err)
	}

	pasted := "-----BEGIN CERTIFICATE-----\nPASTED\n"
	if err := s.UpdateServer(ctx, cluster.UID, ServerUpdate{K8sCACert: &pasted}, key); err != nil {
		t.Fatalf("UpdateServer(paste a bundle) error = %v", err)
	}

	reloaded, err = s.GetServerByUID(ctx, cluster.UID)
	if err != nil {
		t.Fatalf("GetServerByUID() error = %v", err)
	}

	if kd = reloaded.KubernetesData(); kd.CACert != pasted || kd.LearnedCACert != "" {
		t.Errorf("after pasting a bundle: %+v, want the pasted CA and no learned pin", kd)
	}
}

func containsUID(servers []Server, uid interface{ String() string }) bool {
	for i := range servers {
		if servers[i].UID.String() == uid.String() {
			return true
		}
	}

	return false
}
