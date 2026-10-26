package controller

import (
	"context"
	"crypto/tls"
	"fmt"
	"strings"
	"time"

	configv1 "github.com/openshift/api/config/v1"
	libgocrypto "github.com/openshift/library-go/pkg/crypto"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/rest"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/metrics/filters"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	framewtls "github.com/opendatahub-io/odh-platform-utilities/framework/tls"
)

const tlsBootstrapTimeout = 10 * time.Second

// ResolvedTLSConfig is the cluster TLS policy projected into the operand and
// applied to the operator's own metrics listener. ProfileSpec is the raw
// cluster spec and seeds the SecurityProfileWatcher's initial state;
// EffectiveSpec is the profile actually applied after adherence selection.
type ResolvedTLSConfig struct {
	MinVersion            string
	CipherSuites          string
	Groups                string
	UsedTransientFallback bool
	ProfileSpec           configv1.TLSProfileSpec
	EffectiveSpec         configv1.TLSProfileSpec
	Adherence             configv1.TLSAdherencePolicy
	APIAvailable          bool
}

// ResolveTLSConfig resolves the cluster TLS policy through the shared ODH
// framework: the APIServer profile and adherence policy, with Intermediate
// fallback for missing or transiently unreadable APIs. Non-adhering policies
// keep the hardened Intermediate defaults.
func ResolveTLSConfig(ctx context.Context, cl client.Client) (ResolvedTLSConfig, error) {
	log := logf.FromContext(ctx)

	loaded, err := framewtls.LoadWithAdherence(ctx, cl)
	if err != nil {
		return ResolvedTLSConfig{}, fmt.Errorf("fetching TLS profile: %w", err)
	}
	if loaded.UsedFallback {
		log.Info("TLS profile unavailable, using hardened defaults", "transient", loaded.Watchable)
	}

	result := ResolvedTLSConfig{
		ProfileSpec:           loaded.Spec,
		Adherence:             loaded.AdherencePolicy,
		APIAvailable:          loaded.Watchable,
		UsedTransientFallback: loaded.UsedFallback && loaded.Watchable,
		EffectiveSpec:         loaded.Spec,
	}

	// Non-adhering policies keep hardened Intermediate defaults instead of the
	// cluster profile.
	if !framewtls.ShouldHonorClusterTLSProfile(loaded.AdherencePolicy) {
		result.EffectiveSpec = intermediateProfileSpec()
	}

	spec := result.EffectiveSpec
	result.MinVersion = framewtls.MinVersionFromSpec(ctx, &spec, framewtls.FormatGo)
	result.CipherSuites = framewtls.CipherSuitesFromSpec(ctx, &spec)
	if spec.MinTLSVersion == configv1.VersionTLS13 {
		// Go cannot configure TLS 1.3 cipher suites, so none are projected.
		result.CipherSuites = ""
	}

	groups, err := tlsGroupNamesFromSpec(spec)
	if err != nil {
		return ResolvedTLSConfig{}, err
	}
	result.Groups = groups

	return result, nil
}

// tlsGroupNamesFromSpec returns the profile's supported TLS group names,
// comma-separated. Groups the Go runtime cannot use are dropped; a non-empty
// group list with no supported groups is rejected. The framework returns
// CurveID values, but the operand consumes group names.
func tlsGroupNamesFromSpec(spec configv1.TLSProfileSpec) (string, error) {
	if len(spec.Groups) == 0 {
		return "", nil
	}

	names := make([]string, 0, len(spec.Groups))
	for _, group := range spec.Groups {
		if curves, _ := libgocrypto.TLSGroupsToCurveIDs([]configv1.TLSGroup{group}); len(curves) > 0 {
			names = append(names, string(group))
		}
	}
	if len(names) == 0 {
		return "", fmt.Errorf("TLS profile has no usable groups")
	}

	return strings.Join(names, ","), nil
}

func intermediateProfileSpec() configv1.TLSProfileSpec {
	return *configv1.TLSProfiles[configv1.TLSProfileIntermediateType]
}

func FetchTLSConfig(ctx context.Context, cl client.Client) (string, string, string, error) {
	result, err := ResolveTLSConfig(ctx, cl)
	return result.MinVersion, result.CipherSuites, result.Groups, err
}

func fetchTLSConfig(ctx context.Context, cl client.Client) (string, string, string, error) {
	return FetchTLSConfig(ctx, cl)
}

// MetricsOptions resolves the cluster TLS policy before the manager starts and
// returns secure controller-runtime metrics options using the same policy that
// is projected into the operand.
func MetricsOptions(ctx context.Context, cfg *rest.Config) (metricsserver.Options, error) {
	options, _, err := MetricsOptionsWithTLSConfig(ctx, cfg)
	return options, err
}

// MetricsOptionsWithTLSConfig resolves secure metrics options and returns the
// profile metadata needed to register the TLS watcher after manager setup.
func MetricsOptionsWithTLSConfig(ctx context.Context, cfg *rest.Config) (metricsserver.Options, ResolvedTLSConfig, error) {
	scheme := runtime.NewScheme()
	if err := configv1.Install(scheme); err != nil {
		return metricsserver.Options{}, ResolvedTLSConfig{}, fmt.Errorf("installing OpenShift config scheme: %w", err)
	}
	bootstrapClient, err := client.New(cfg, client.Options{Scheme: scheme})
	if err != nil {
		return metricsserver.Options{}, ResolvedTLSConfig{}, fmt.Errorf("creating TLS bootstrap client: %w", err)
	}
	bootstrapCtx, cancel := context.WithTimeout(ctx, tlsBootstrapTimeout)
	defer cancel()
	resolved, err := resolveMetricsTLSConfig(bootstrapCtx, bootstrapClient, metricsRetryBackoff)
	if err != nil {
		return metricsserver.Options{}, ResolvedTLSConfig{}, err
	}
	options, err := metricsOptionsFromResolved(resolved)
	if err != nil {
		return metricsserver.Options{}, ResolvedTLSConfig{}, err
	}
	return options, resolved, nil
}

var metricsRetryBackoff = wait.Backoff{Duration: time.Second, Factor: 2, Steps: 3}

// resolveMetricsTLSConfig retries transient APIServer read failures with
// backoff. Persistent transient failures return the Intermediate fallback with
// UsedTransientFallback set; non-transient errors fail closed.
func resolveMetricsTLSConfig(ctx context.Context, cl client.Client, backoff wait.Backoff) (ResolvedTLSConfig, error) {
	var resolved ResolvedTLSConfig
	var resolveErr error
	retryErr := wait.ExponentialBackoffWithContext(ctx, backoff, func(ctx context.Context) (bool, error) {
		resolved, resolveErr = ResolveTLSConfig(ctx, cl)
		if resolveErr != nil {
			return false, resolveErr
		}
		return !resolved.UsedTransientFallback, nil
	})
	if resolveErr != nil {
		return ResolvedTLSConfig{}, fmt.Errorf("resolving TLS configuration: %w", resolveErr)
	}
	if retryErr != nil && !resolved.UsedTransientFallback {
		return ResolvedTLSConfig{}, fmt.Errorf("resolving TLS configuration: %w", retryErr)
	}
	return resolved, nil
}

func metricsOptionsFromResolved(resolved ResolvedTLSConfig) (metricsserver.Options, error) {
	configure, _, err := framewtls.ConfigFromProfileStrict(resolved.EffectiveSpec)
	if err != nil {
		return metricsserver.Options{}, fmt.Errorf("building metrics TLS options: %w", err)
	}

	return metricsserver.Options{
		BindAddress:    ":8443",
		SecureServing:  true,
		FilterProvider: filters.WithAuthenticationAndAuthorization,
		TLSOpts: []func(*tls.Config){
			configure,
			// Serve both HTTP/2 and HTTP/1.1 on the metrics listener.
			func(c *tls.Config) { c.NextProtos = []string{"h2", "http/1.1"} },
		},
	}, nil
}

func isOpenShiftCluster(mgr ctrl.Manager) bool {
	_, err := mgr.GetRESTMapper().RESTMapping(
		schema.GroupKind{Group: "config.openshift.io", Kind: "APIServer"},
	)
	return err == nil
}
