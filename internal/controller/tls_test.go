package controller

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	configv1 "github.com/openshift/api/config/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
)

var tlsTestScheme = func() *runtime.Scheme {
	s := runtime.NewScheme()
	utilruntime.Must(configv1.Install(s))
	return s
}()

func TestFetchTLSConfig_IntermediateProfile(t *testing.T) {
	apiServer := &configv1.APIServer{
		ObjectMeta: metav1.ObjectMeta{Name: "cluster"},
		Spec: configv1.APIServerSpec{
			TLSAdherence: configv1.TLSAdherencePolicyStrictAllComponents,
			TLSSecurityProfile: &configv1.TLSSecurityProfile{
				Type: configv1.TLSProfileIntermediateType,
			},
		},
	}

	cli := fake.NewClientBuilder().
		WithScheme(tlsTestScheme).
		WithObjects(apiServer).
		Build()

	minVersion, cipherSuites, _, err := fetchTLSConfig(context.Background(), cli)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if minVersion != "VersionTLS12" {
		t.Errorf("minVersion = %q, want %q", minVersion, "VersionTLS12")
	}

	ciphers := strings.Split(cipherSuites, ",")
	if len(ciphers) == 0 {
		t.Fatal("expected non-empty cipher suites")
	}

	expectedCiphers := []string{
		"TLS_ECDHE_ECDSA_WITH_AES_128_GCM_SHA256",
		"TLS_ECDHE_RSA_WITH_AES_128_GCM_SHA256",
		"TLS_ECDHE_ECDSA_WITH_AES_256_GCM_SHA384",
		"TLS_ECDHE_RSA_WITH_AES_256_GCM_SHA384",
		"TLS_ECDHE_ECDSA_WITH_CHACHA20_POLY1305_SHA256",
		"TLS_ECDHE_RSA_WITH_CHACHA20_POLY1305_SHA256",
	}
	for _, expected := range expectedCiphers {
		if !strings.Contains(cipherSuites, expected) {
			t.Errorf("cipher suites missing %q", expected)
		}
	}
}

func TestFetchTLSConfig_ModernProfile(t *testing.T) {
	apiServer := &configv1.APIServer{
		ObjectMeta: metav1.ObjectMeta{Name: "cluster"},
		Spec: configv1.APIServerSpec{
			TLSAdherence: configv1.TLSAdherencePolicyStrictAllComponents,
			TLSSecurityProfile: &configv1.TLSSecurityProfile{
				Type: configv1.TLSProfileModernType,
			},
		},
	}

	cli := fake.NewClientBuilder().
		WithScheme(tlsTestScheme).
		WithObjects(apiServer).
		Build()

	minVersion, cipherSuites, _, err := fetchTLSConfig(context.Background(), cli)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if minVersion != "VersionTLS13" {
		t.Errorf("minVersion = %q, want %q", minVersion, "VersionTLS13")
	}

	if cipherSuites != "" {
		t.Errorf("expected no TLS 1.3 cipher flag, got %q", cipherSuites)
	}
}

func TestFetchTLSConfig_CustomProfile(t *testing.T) {
	apiServer := &configv1.APIServer{
		ObjectMeta: metav1.ObjectMeta{Name: "cluster"},
		Spec: configv1.APIServerSpec{
			TLSAdherence: configv1.TLSAdherencePolicyStrictAllComponents,
			TLSSecurityProfile: &configv1.TLSSecurityProfile{
				Type: configv1.TLSProfileCustomType,
				Custom: &configv1.CustomTLSProfile{
					TLSProfileSpec: configv1.TLSProfileSpec{
						MinTLSVersion: configv1.VersionTLS13,
						Ciphers:       []string{"TLS_AES_128_GCM_SHA256"},
					},
				},
			},
		},
	}

	cli := fake.NewClientBuilder().
		WithScheme(tlsTestScheme).
		WithObjects(apiServer).
		Build()

	minVersion, cipherSuites, _, err := fetchTLSConfig(context.Background(), cli)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if minVersion != "VersionTLS13" {
		t.Errorf("minVersion = %q, want %q", minVersion, "VersionTLS13")
	}

	if cipherSuites != "" {
		t.Errorf("expected no TLS 1.3 cipher flag, got %q", cipherSuites)
	}
}

func TestFetchTLSConfig_NilProfile_ReturnsIntermediateDefaults(t *testing.T) {
	apiServer := &configv1.APIServer{
		ObjectMeta: metav1.ObjectMeta{Name: "cluster"},
	}

	cli := fake.NewClientBuilder().
		WithScheme(tlsTestScheme).
		WithObjects(apiServer).
		Build()

	minVersion, cipherSuites, _, err := fetchTLSConfig(context.Background(), cli)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if minVersion != "VersionTLS12" {
		t.Errorf("minVersion = %q, want %q (Intermediate default)", minVersion, "VersionTLS12")
	}

	if cipherSuites == "" {
		t.Error("expected non-empty cipher suites for Intermediate defaults")
	}
}

func TestFetchTLSConfig_APIServerNotFound_ReturnsDefaults(t *testing.T) {
	cli := fake.NewClientBuilder().
		WithScheme(tlsTestScheme).
		Build()

	minVersion, cipherSuites, _, err := fetchTLSConfig(context.Background(), cli)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if minVersion != "VersionTLS12" {
		t.Errorf("minVersion = %q, want %q (Intermediate default)", minVersion, "VersionTLS12")
	}

	if cipherSuites == "" {
		t.Error("expected non-empty cipher suites for Intermediate defaults")
	}
}

func TestResolveTLSConfig_TransientErrorReturnsDefaults(t *testing.T) {
	cli := fake.NewClientBuilder().
		WithScheme(tlsTestScheme).
		WithInterceptorFuncs(interceptor.Funcs{
			Get: func(context.Context, client.WithWatch, client.ObjectKey, client.Object, ...client.GetOption) error {
				return apierrors.NewServiceUnavailable("temporary API server failure")
			},
		}).
		Build()

	resolved, err := ResolveTLSConfig(context.Background(), cli)
	if err != nil {
		t.Fatal(err)
	}
	if !resolved.UsedTransientFallback || !resolved.APIAvailable {
		t.Fatalf("transient fallback flags = UsedTransientFallback %t APIAvailable %t, want both true", resolved.UsedTransientFallback, resolved.APIAvailable)
	}
	if resolved.MinVersion != string(configv1.VersionTLS12) || resolved.CipherSuites == "" {
		t.Fatalf("transient fallback = min %q ciphers %q, want Intermediate defaults", resolved.MinVersion, resolved.CipherSuites)
	}
}

func TestResolveMetricsTLSConfigRetriesTransientThenSucceeds(t *testing.T) {
	apiServer := &configv1.APIServer{
		ObjectMeta: metav1.ObjectMeta{Name: "cluster"},
		Spec: configv1.APIServerSpec{
			TLSAdherence:       configv1.TLSAdherencePolicyStrictAllComponents,
			TLSSecurityProfile: &configv1.TLSSecurityProfile{Type: configv1.TLSProfileModernType},
		},
	}
	var calls int32
	cli := fake.NewClientBuilder().
		WithScheme(tlsTestScheme).
		WithObjects(apiServer).
		WithInterceptorFuncs(interceptor.Funcs{
			Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
				if atomic.AddInt32(&calls, 1) == 1 {
					return apierrors.NewServiceUnavailable("temporary API server failure")
				}
				return c.Get(ctx, key, obj, opts...)
			},
		}).
		Build()

	resolved, err := resolveMetricsTLSConfig(context.Background(), cli, wait.Backoff{Duration: time.Millisecond, Factor: 1, Steps: 3})
	if err != nil {
		t.Fatal(err)
	}
	if resolved.UsedTransientFallback || resolved.MinVersion != string(configv1.VersionTLS13) {
		t.Fatalf("retry result = %+v, want successful Modern profile resolution without fallback", resolved)
	}
}

func TestResolveMetricsTLSConfigPersistentTransientReturnsFallback(t *testing.T) {
	cli := fake.NewClientBuilder().
		WithScheme(tlsTestScheme).
		WithInterceptorFuncs(interceptor.Funcs{
			Get: func(context.Context, client.WithWatch, client.ObjectKey, client.Object, ...client.GetOption) error {
				return apierrors.NewServiceUnavailable("temporary API server failure")
			},
		}).
		Build()

	resolved, err := resolveMetricsTLSConfig(context.Background(), cli, wait.Backoff{Duration: time.Millisecond, Factor: 1, Steps: 2})
	if err != nil {
		t.Fatal(err)
	}
	if !resolved.UsedTransientFallback || resolved.MinVersion != string(configv1.VersionTLS12) {
		t.Fatalf("persistent transient result = %+v, want Intermediate fallback with UsedTransientFallback", resolved)
	}
}

func TestResolveMetricsTLSConfigFailsClosedOnForbidden(t *testing.T) {
	cli := fake.NewClientBuilder().
		WithScheme(tlsTestScheme).
		WithInterceptorFuncs(interceptor.Funcs{
			Get: func(context.Context, client.WithWatch, client.ObjectKey, client.Object, ...client.GetOption) error {
				return apierrors.NewForbidden(schema.GroupResource{Group: "config.openshift.io", Resource: "apiservers"}, "cluster", errors.New("denied"))
			},
		}).
		Build()

	if _, err := resolveMetricsTLSConfig(context.Background(), cli, wait.Backoff{Duration: time.Millisecond, Factor: 1, Steps: 2}); err == nil {
		t.Fatal("expected forbidden APIServer read to fail closed")
	}
}

func TestFetchTLSConfig_CustomProfile_WithGroups(t *testing.T) {
	apiServer := &configv1.APIServer{
		ObjectMeta: metav1.ObjectMeta{Name: "cluster"},
		Spec: configv1.APIServerSpec{
			TLSAdherence: configv1.TLSAdherencePolicyStrictAllComponents,
			TLSSecurityProfile: &configv1.TLSSecurityProfile{
				Type: configv1.TLSProfileCustomType,
				Custom: &configv1.CustomTLSProfile{
					TLSProfileSpec: configv1.TLSProfileSpec{
						MinTLSVersion: configv1.VersionTLS13,
						Ciphers:       []string{"TLS_AES_128_GCM_SHA256"},
						Groups: []configv1.TLSGroup{
							"X25519MLKEM768",
							"X25519",
							"secp256r1",
						},
					},
				},
			},
		},
	}

	cli := fake.NewClientBuilder().
		WithScheme(tlsTestScheme).
		WithObjects(apiServer).
		Build()

	_, _, groups, err := fetchTLSConfig(context.Background(), cli)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if groups != "X25519MLKEM768,X25519,secp256r1" {
		t.Errorf("groups = %q, want %q", groups, "X25519MLKEM768,X25519,secp256r1")
	}
}

func TestFetchTLSConfig_LegacyUsesIntermediateDefaults(t *testing.T) {
	apiServer := &configv1.APIServer{
		ObjectMeta: metav1.ObjectMeta{Name: "cluster"},
		Spec: configv1.APIServerSpec{
			TLSAdherence:       configv1.TLSAdherencePolicyLegacyAdheringComponentsOnly,
			TLSSecurityProfile: &configv1.TLSSecurityProfile{Type: configv1.TLSProfileModernType},
		},
	}
	cli := fake.NewClientBuilder().WithScheme(tlsTestScheme).WithObjects(apiServer).Build()
	minVersion, cipherSuites, _, err := fetchTLSConfig(context.Background(), cli)
	if err != nil {
		t.Fatal(err)
	}
	if minVersion != string(configv1.VersionTLS12) || cipherSuites == "" {
		t.Fatalf("Legacy result = min %q ciphers %q, want Intermediate defaults", minVersion, cipherSuites)
	}
}

func TestFetchTLSConfig_NoOpinionUsesIntermediateDefaults(t *testing.T) {
	apiServer := &configv1.APIServer{
		ObjectMeta: metav1.ObjectMeta{Name: "cluster"},
		Spec: configv1.APIServerSpec{
			TLSSecurityProfile: &configv1.TLSSecurityProfile{Type: configv1.TLSProfileModernType},
		},
	}
	cli := fake.NewClientBuilder().WithScheme(tlsTestScheme).WithObjects(apiServer).Build()
	minVersion, cipherSuites, _, err := fetchTLSConfig(context.Background(), cli)
	if err != nil {
		t.Fatal(err)
	}
	if minVersion != string(configv1.VersionTLS12) || cipherSuites == "" {
		t.Fatalf("NoOpinion result = min %q ciphers %q, want Intermediate defaults", minVersion, cipherSuites)
	}
}

func TestFetchTLSConfig_UnknownAdherenceHonorsProfile(t *testing.T) {
	apiServer := &configv1.APIServer{
		ObjectMeta: metav1.ObjectMeta{Name: "cluster"},
		Spec: configv1.APIServerSpec{
			TLSAdherence:       "FutureAdherence",
			TLSSecurityProfile: &configv1.TLSSecurityProfile{Type: configv1.TLSProfileModernType},
		},
	}
	cli := fake.NewClientBuilder().WithScheme(tlsTestScheme).WithObjects(apiServer).Build()
	minVersion, _, _, err := fetchTLSConfig(context.Background(), cli)
	if err != nil {
		t.Fatal(err)
	}
	if minVersion != string(configv1.VersionTLS13) {
		t.Fatalf("unknown adherence minimum = %q, want %q", minVersion, configv1.VersionTLS13)
	}
}

func TestFetchTLSConfig_StrictRejectsInvalidProfiles(t *testing.T) {
	tests := []struct {
		name    string
		profile *configv1.TLSSecurityProfile
	}{
		{
			name: "invalid minimum",
			profile: &configv1.TLSSecurityProfile{Type: configv1.TLSProfileCustomType,
				Custom: &configv1.CustomTLSProfile{TLSProfileSpec: configv1.TLSProfileSpec{MinTLSVersion: "VersionTLS99"}}},
		},
		{
			name:    "unknown type",
			profile: &configv1.TLSSecurityProfile{Type: "FutureProfile"},
		},
		{
			name: "all unusable ciphers",
			profile: &configv1.TLSSecurityProfile{Type: configv1.TLSProfileCustomType,
				Custom: &configv1.CustomTLSProfile{TLSProfileSpec: configv1.TLSProfileSpec{MinTLSVersion: configv1.VersionTLS12, Ciphers: []string{"TLS_AES_128_GCM_SHA256"}}}},
		},
		{
			name: "no usable groups",
			profile: &configv1.TLSSecurityProfile{Type: configv1.TLSProfileCustomType,
				Custom: &configv1.CustomTLSProfile{TLSProfileSpec: configv1.TLSProfileSpec{
					MinTLSVersion: configv1.VersionTLS12,
					Ciphers:       []string{"ECDHE-RSA-AES128-GCM-SHA256"},
					Groups:        []configv1.TLSGroup{"FutureGroup"},
				}}},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			apiServer := &configv1.APIServer{ObjectMeta: metav1.ObjectMeta{Name: "cluster"}, Spec: configv1.APIServerSpec{
				TLSAdherence: configv1.TLSAdherencePolicyStrictAllComponents, TLSSecurityProfile: tt.profile,
			}}
			cli := fake.NewClientBuilder().WithScheme(tlsTestScheme).WithObjects(apiServer).Build()
			if _, _, _, err := fetchTLSConfig(context.Background(), cli); err == nil {
				t.Fatal("expected strict profile error")
			}
		})
	}
}

func TestFetchTLSConfig_DropsRC4AndRetainsUsableCipher(t *testing.T) {
	apiServer := &configv1.APIServer{ObjectMeta: metav1.ObjectMeta{Name: "cluster"}, Spec: configv1.APIServerSpec{
		TLSAdherence: configv1.TLSAdherencePolicyStrictAllComponents,
		TLSSecurityProfile: &configv1.TLSSecurityProfile{Type: configv1.TLSProfileCustomType,
			Custom: &configv1.CustomTLSProfile{TLSProfileSpec: configv1.TLSProfileSpec{MinTLSVersion: configv1.VersionTLS12,
				Ciphers: []string{"TLS_RSA_WITH_RC4_128_SHA", "ECDHE-RSA-AES128-GCM-SHA256"}}}},
	}}
	cli := fake.NewClientBuilder().WithScheme(tlsTestScheme).WithObjects(apiServer).Build()
	_, ciphers, _, err := fetchTLSConfig(context.Background(), cli)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(ciphers, "RC4") || !strings.Contains(ciphers, "TLS_ECDHE_RSA_WITH_AES_128_GCM_SHA256") {
		t.Fatalf("ciphers = %q, want RC4 excluded and usable suite retained", ciphers)
	}
}

func TestMetricsOptionsFromResolved(t *testing.T) {
	resolved := ResolvedTLSConfig{EffectiveSpec: *configv1.TLSProfiles[configv1.TLSProfileIntermediateType]}
	options, err := metricsOptionsFromResolved(resolved)
	if err != nil {
		t.Fatal(err)
	}
	if !options.SecureServing || options.BindAddress != ":8443" || options.FilterProvider == nil {
		t.Fatalf("metrics options = %+v, want secure serving on :8443 with authn/authz filter", options)
	}

	cfg := &tls.Config{}
	for _, opt := range options.TLSOpts {
		opt(cfg)
	}
	if cfg.MinVersion != tls.VersionTLS12 {
		t.Errorf("metrics TLS min version = %d, want TLS 1.2", cfg.MinVersion)
	}
	if len(cfg.CipherSuites) == 0 {
		t.Error("metrics TLS cipher suites are empty, want the Intermediate profile suites")
	}
	var servesHTTP2 bool
	for _, proto := range cfg.NextProtos {
		if proto == "h2" {
			servesHTTP2 = true
		}
	}
	if !servesHTTP2 {
		t.Errorf("metrics TLS NextProtos = %v, want h2", cfg.NextProtos)
	}
}

// metricsOptionsFromClient is a test-only convenience wrapper mirroring the
// production path (resolve + metricsOptionsFromResolved) without the retry
// loop, so fallback and fail-closed cases can be asserted in a single call.
func metricsOptionsFromClient(ctx context.Context, cl client.Client) (metricsserver.Options, error) {
	resolved, err := ResolveTLSConfig(ctx, cl)
	if err != nil {
		return metricsserver.Options{}, fmt.Errorf("resolving TLS configuration: %w", err)
	}
	return metricsOptionsFromResolved(resolved)
}

func TestMetricsOptionsFromClientUsesFallbackForMissingAPIServer(t *testing.T) {
	cli := fake.NewClientBuilder().WithScheme(tlsTestScheme).Build()
	opts, err := metricsOptionsFromClient(context.Background(), cli)
	if err != nil {
		t.Fatal(err)
	}
	if opts.BindAddress != ":8443" || !opts.SecureServing || opts.FilterProvider == nil || len(opts.TLSOpts) == 0 {
		t.Fatalf("metrics options = %+v, want secure authenticated fallback", opts)
	}
}

func TestMetricsOptionsFromClientUsesFallbackWhenConfigAPIIsMissing(t *testing.T) {
	cli := fake.NewClientBuilder().
		WithScheme(tlsTestScheme).
		WithInterceptorFuncs(interceptor.Funcs{
			Get: func(context.Context, client.WithWatch, client.ObjectKey, client.Object, ...client.GetOption) error {
				return &meta.NoKindMatchError{
					GroupKind: schema.GroupKind{Group: "config.openshift.io", Kind: "APIServer"},
				}
			},
		}).
		Build()
	opts, err := metricsOptionsFromClient(context.Background(), cli)
	if err != nil {
		t.Fatal(err)
	}
	if opts.BindAddress != ":8443" || !opts.SecureServing || opts.FilterProvider == nil || len(opts.TLSOpts) == 0 {
		t.Fatalf("metrics options = %+v, want secure authenticated fallback", opts)
	}
}

func TestMetricsOptionsFromClientFailsClosedOnDeniedAPIServer(t *testing.T) {
	cli := fake.NewClientBuilder().WithScheme(tlsTestScheme).WithInterceptorFuncs(interceptor.Funcs{
		Get: func(context.Context, client.WithWatch, client.ObjectKey, client.Object, ...client.GetOption) error {
			return apierrors.NewForbidden(schema.GroupResource{Group: "config.openshift.io", Resource: "apiservers"}, "cluster", errors.New("denied"))
		},
	}).Build()
	if _, err := metricsOptionsFromClient(context.Background(), cli); err == nil {
		t.Fatal("expected denied APIServer read to fail closed")
	}
}

func TestFetchTLSConfig_NoMatchError_ReturnsDefaults(t *testing.T) {
	cli := fake.NewClientBuilder().
		WithScheme(tlsTestScheme).
		WithInterceptorFuncs(interceptor.Funcs{
			Get: func(context.Context, client.WithWatch, client.ObjectKey, client.Object, ...client.GetOption) error {
				return &meta.NoKindMatchError{
					GroupKind: schema.GroupKind{Group: "config.openshift.io", Kind: "APIServer"},
				}
			},
		}).
		Build()

	minVersion, cipherSuites, _, err := fetchTLSConfig(context.Background(), cli)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if minVersion != "VersionTLS12" {
		t.Errorf("minVersion = %q, want %q (Intermediate default)", minVersion, "VersionTLS12")
	}

	if cipherSuites == "" {
		t.Error("expected non-empty cipher suites for Intermediate defaults")
	}
}

// TestFetchTLSConfig_NotRegisteredError_FailsClosed covers a client scheme
// that does not register configv1.APIServer: this is a local wiring bug, not
// an absent API, so the resolver must fail closed instead of silently
// serving the Intermediate fallback.
func TestFetchTLSConfig_NotRegisteredError_FailsClosed(t *testing.T) {
	cli := fake.NewClientBuilder().
		WithScheme(runtime.NewScheme()).
		Build()

	if _, _, _, err := fetchTLSConfig(context.Background(), cli); err == nil {
		t.Fatal("expected not-registered APIServer read to fail closed")
	}
}
