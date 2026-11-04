package snapshot_test

import (
	"strings"
	"testing"

	"github.com/rbalukja15/k8s-doctor/internal/snapshot"
)

func TestDecodeRejectsUnknownFields(t *testing.T) {
	_, err := snapshot.Decode(strings.NewReader(`{"schemaVersion":"v1alpha1","mode":"manifest","spec":{"workloadz":[]}}`))
	if err == nil || !strings.Contains(err.Error(), "workloadz") {
		t.Fatalf("expected an unknown-field error, got %v", err)
	}
}

func TestValidate(t *testing.T) {
	cases := map[string]struct {
		json    string
		wantErr string
	}{
		"valid manifest":           {`{"schemaVersion":"v1alpha1","mode":"manifest","spec":{}}`, ""},
		"valid cluster":            {`{"schemaVersion":"v1alpha1","mode":"cluster","spec":{},"runtime":{}}`, ""},
		"wrong version":            {`{"schemaVersion":"v0","mode":"manifest","spec":{}}`, "schemaVersion"},
		"bad mode":                 {`{"schemaVersion":"v1alpha1","mode":"live","spec":{}}`, "invalid mode"},
		"runtime in manifest mode": {`{"schemaVersion":"v1alpha1","mode":"manifest","spec":{},"runtime":{}}`, "must be absent"},
		"cluster without runtime":  {`{"schemaVersion":"v1alpha1","mode":"cluster","spec":{}}`, "required in cluster mode"},
		"bad workload kind": {
			`{"schemaVersion":"v1alpha1","mode":"manifest","spec":{"workloads":[{"kind":"Pod","apiVersion":"v1","name":"x","template":{"spec":{"containers":[]}}}]}}`,
			"unsupported kind",
		},
	}
	for name, c := range cases {
		_, err := snapshot.Decode(strings.NewReader(c.json))
		switch {
		case c.wantErr == "" && err != nil:
			t.Errorf("%s: unexpected error %v", name, err)
		case c.wantErr != "" && (err == nil || !strings.Contains(err.Error(), c.wantErr)):
			t.Errorf("%s: error %v, want one containing %q", name, err, c.wantErr)
		}
	}
}

func TestDecodeClusterEventWithoutOwner(t *testing.T) {
	data := `{"schemaVersion":"v1alpha1","mode":"cluster","spec":{},"runtime":{"events":[{"involvedObject":{"kind":"ReplicaSet","namespace":"shop","name":"api-7d9f"},"type":"Warning","reason":"FailedCreate"}]}}`
	decoded, err := snapshot.Decode(strings.NewReader(data))
	if err != nil {
		t.Fatalf("decode legacy event without owner: %v", err)
	}
	if got := decoded.Runtime.Events[0].Owner; got != nil {
		t.Fatalf("legacy event owner = %+v, want nil", got)
	}
}

func TestResourceRefRoundTrip(t *testing.T) {
	for _, s := range []string{"Deployment/shop/api", "Namespace/shop"} {
		ref, err := snapshot.ParseResourceRef(s)
		if err != nil {
			t.Fatal(err)
		}
		if ref.String() != s {
			t.Errorf("round trip of %q gave %q", s, ref.String())
		}
	}
	for _, bad := range []string{"", "Deployment", "a//b", "a/b/c/d"} {
		if _, err := snapshot.ParseResourceRef(bad); err == nil {
			t.Errorf("ParseResourceRef(%q): expected an error", bad)
		}
	}
}

func TestEffectiveReplicas(t *testing.T) {
	two := int32(2)
	cases := []struct {
		w    snapshot.Workload
		want int32
		ok   bool
	}{
		{snapshot.Workload{Kind: snapshot.KindDeployment}, 1, true},
		{snapshot.Workload{Kind: snapshot.KindStatefulSet, Replicas: &two}, 2, true},
		{snapshot.Workload{Kind: snapshot.KindDaemonSet}, 0, false},
	}
	for _, c := range cases {
		got, ok := c.w.EffectiveReplicas()
		if got != c.want || ok != c.ok {
			t.Errorf("%s: EffectiveReplicas() = %d, %v; want %d, %v", c.w.Kind, got, ok, c.want, c.ok)
		}
	}
}
