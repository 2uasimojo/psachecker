package checker

import (
	"context"
	"fmt"
	"sort"

	"github.com/spf13/cobra"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/cli-runtime/pkg/genericclioptions"
	"k8s.io/cli-runtime/pkg/resource"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	cmdutil "k8s.io/kubectl/pkg/cmd/util"
	psapi "k8s.io/pod-security-admission/api"
)

var (
	scheme = runtime.NewScheme()
)

func init() {
	utilruntime.Must(corev1.AddToScheme(scheme))
	utilruntime.Must(appsv1.AddToScheme(scheme))
	utilruntime.Must(batchv1.AddToScheme(scheme))
}

type PSACheckerOptions struct {
	ClientConfigOptions *genericclioptions.ConfigFlags
	filenameOptions     *resource.FilenameOptions

	// custom flags
	defaultNamespaces bool
	inspectCluster    bool
	updatesOnly       bool

	builder    *resource.Builder
	kubeClient kubernetes.Interface

	isLocal bool
}

func NewPSACheckerOptions() *PSACheckerOptions {
	return &PSACheckerOptions{
		ClientConfigOptions: genericclioptions.NewConfigFlags(true),
		filenameOptions:     &resource.FilenameOptions{},
	}
}

func (opts *PSACheckerOptions) AddFlags(cmd *cobra.Command) {
	flags := cmd.Flags()

	opts.ClientConfigOptions.AddFlags(cmd.Flags())
	cmdutil.AddFilenameOptionFlags(cmd, // TODO: this adds a kustomize flag, do we need to special-case handle it?
		opts.filenameOptions,
		"identifying the resource to run PodSecurity admission check against",
	)
	flags.BoolVar(&opts.defaultNamespaces, "default-namespaces", false, "Default empty namespaces in files to the --namespace value.")
	cmd.PersistentFlags().BoolVar(&opts.updatesOnly, "updates-only", false, "Display only namespaces that need to be updated. Does not currently work for local files.")
}

func (opts *PSACheckerOptions) ClientConfig() (*rest.Config, error) {
	return opts.ClientConfigOptions.ToRawKubeConfigLoader().ClientConfig()
}

func (opts *PSACheckerOptions) Complete(args []string) error {
	clientConfig, err := opts.ClientConfigOptions.ToRawKubeConfigLoader().ClientConfig()
	if err != nil {
		return err
	}

	opts.kubeClient, err = kubernetes.NewForConfig(clientConfig)
	if err != nil {
		return err
	}

	opts.builder = resource.NewBuilder(opts.ClientConfigOptions).
		WithScheme(scheme,
			corev1.SchemeGroupVersion,
			appsv1.SchemeGroupVersion,
			batchv1.SchemeGroupVersion,
		)

	// make the builder accept files if provided, otherwise expect resourceType and name
	if files := opts.filenameOptions.Filenames; len(files) > 0 {
		opts.builder = opts.builder.
			Local().
			FilenameParam(false, opts.filenameOptions)

		opts.isLocal = true
	} else {
		opts.builder = opts.builder.
			SingleResourceType().
			ResourceTypeOrNameArgs(true, args...)
	}

	if ns := *opts.ClientConfigOptions.Namespace; len(ns) > 0 {
		opts.builder = opts.builder.
			NamespaceParam(ns).
			DefaultNamespace()
	}

	return nil
}

func (opts *PSACheckerOptions) Validate() []error {
	errs := []error{}

	if opts.kubeClient == nil {
		errs = append(errs, fmt.Errorf("missing kube client"))
	}

	if opts.defaultNamespaces && len(*opts.ClientConfigOptions.Namespace) == 0 {
		errs = append(errs, fmt.Errorf("cannot specify --default-namespaces without also providing a value for --namespace"))
	}

	if opts.inspectCluster {
		if len(*opts.ClientConfigOptions.Namespace) > 0 {
			errs = append(errs, fmt.Errorf("cannot specify --inspect-cluster along with --namespace"))
		}
		if len(opts.filenameOptions.Filenames) > 0 {
			errs = append(errs, fmt.Errorf("cannot specify local files when --inspect-cluster is set"))
		}
	}

	return errs
}

func (opts *PSACheckerOptions) Run(ctx context.Context) (*OrderedStringToPSALevelMap, error) {
	adm, err := NewParallelAdmission(opts.kubeClient)
	if err != nil {
		return nil, fmt.Errorf("failed to set up admission: %w", err)
	}

	var nsAggregatedResults map[string]psapi.Level

	res := opts.builder.Do()

	infos, err := res.Infos()
	if err != nil {
		return nil, fmt.Errorf("failed to retrieve info about the objects: %w", err)
	}

	var defaultNS *string
	if opts.defaultNamespaces {
		defaultNS = opts.ClientConfigOptions.Namespace
	}

	results, err := adm.ValidateResources(ctx, opts.isLocal, defaultNS, infos...)
	if err != nil {
		return nil, err
	}
	nsAggregatedResults = MostRestrictivePolicyPerNamespace(results)
	if !opts.isLocal && opts.updatesOnly {
		// TODO: list the NSes we've got in the map at the same time instead of going 1-by-1?
		for ns, level := range nsAggregatedResults {
			liveNS, err := opts.kubeClient.CoreV1().Namespaces().Get(ctx, ns, metav1.GetOptions{})
			if err != nil {
				return nil, err
			}
			// FIXME: need to take the global config into account
			if string(level) == liveNS.Labels[psapi.EnforceLevelLabel] {
				delete(nsAggregatedResults, ns)
			}
		}
	}

	return NewOrderedStringToPSALevelMap(nsAggregatedResults), nil
}

type OrderedStringToPSALevelMap struct {
	ordered     bool
	internalMap map[string]psapi.Level
	keys        sort.StringSlice
}

func NewOrderedStringToPSALevelMap(m map[string]psapi.Level) *OrderedStringToPSALevelMap {
	ret := &OrderedStringToPSALevelMap{
		ordered:     true,
		internalMap: make(map[string]psapi.Level),
		keys:        make([]string, 0),
	}

	if len(m) != 0 {
		ret.ordered = false
		ret.internalMap = m
		for k := range m {
			ret.keys = append(ret.keys, k)
		}
	}

	return ret
}

func (m *OrderedStringToPSALevelMap) Set(k string, v psapi.Level) {
	if _, ok := m.internalMap[k]; !ok {
		m.ordered = false
		m.keys = append(m.keys, k)
	}
	m.internalMap[k] = v
}

func (m *OrderedStringToPSALevelMap) Get(k string) psapi.Level {
	return m.internalMap[k]
}

func (m *OrderedStringToPSALevelMap) Keys() []string {
	ret := make([]string, len(m.keys))

	if !m.ordered {
		m.keys.Sort()
		m.ordered = true
	}

	copy(ret, m.keys)
	return ret
}
