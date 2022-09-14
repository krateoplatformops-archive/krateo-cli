package cmd

import (
	"context"
	"fmt"
	"io/ioutil"
	"os"
	"strconv"
	"strings"

	"github.com/krateoplatformops/krateo/internal/catalog"
	"github.com/krateoplatformops/krateo/internal/clusterrolebindings"
	"github.com/krateoplatformops/krateo/internal/core"
	"github.com/krateoplatformops/krateo/internal/crossplane"
	"github.com/krateoplatformops/krateo/internal/crossplane/compositeresourcedefinitions"
	"github.com/krateoplatformops/krateo/internal/crossplane/configurations"
	"github.com/krateoplatformops/krateo/internal/crossplane/providers"
	"github.com/krateoplatformops/krateo/internal/eventbus"
	"github.com/krateoplatformops/krateo/internal/events"
	"github.com/krateoplatformops/krateo/internal/helm"
	"github.com/krateoplatformops/krateo/internal/log"
	"github.com/krateoplatformops/krateo/internal/prompt"
	"github.com/spf13/cobra"
	"helm.sh/helm/v3/pkg/strvals"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
)

func newInitCmd() *cobra.Command {
	o := initOpts{
		bus:     eventbus.New(),
		verbose: false,
	}

	cmd := &cobra.Command{
		Use:                   "init",
		DisableSuggestions:    true,
		DisableFlagsInUseLine: false,
		Args:                  cobra.NoArgs,
		Short:                 "Initialize Krateo Platform",
		RunE: func(cmd *cobra.Command, args []string) error {
			l := log.GetInstance()
			if o.verbose {
				l.SetLevel(log.DebugLevel)
			}

			handler := events.LogHandler(l)

			eids := []eventbus.Subscription{
				o.bus.Subscribe(events.StartWaitEventID, handler),
				o.bus.Subscribe(events.StopWaitEventID, handler),
				o.bus.Subscribe(events.DoneEventID, handler),
				o.bus.Subscribe(events.DebugEventID, handler),
			}
			defer func() {
				for _, e := range eids {
					o.bus.Unsubscribe(e)
				}
			}()

			if err := o.complete(); err != nil {
				return err
			}

			return o.run()
		},
	}

	defaultKubeconfig := os.Getenv(clientcmd.RecommendedConfigPathEnvVar)
	if len(defaultKubeconfig) == 0 {
		defaultKubeconfig = clientcmd.RecommendedHomeFile
	}

	cmd.Flags().BoolVarP(&o.verbose, "verbose", "v", false, "dump verbose output")
	cmd.Flags().StringVar(&o.kubeconfig, clientcmd.RecommendedConfigPathFlag, defaultKubeconfig, "absolute path to the kubeconfig file")
	cmd.Flags().StringVar(&o.kubeconfigContext, "context", "", "kubeconfig context to use")
	cmd.Flags().StringVar(&o.httpProxy, "http-proxy", os.Getenv("HTTP_PROXY"), "use the specified HTTP proxy")
	cmd.Flags().StringVar(&o.httpsProxy, "https-proxy", os.Getenv("HTTPS_PROXY"), "use the specified HTTPS proxy")
	cmd.Flags().StringVar(&o.noProxy, "no-proxy", os.Getenv("NO_PROXY"), "comma-separated list of hosts and domains which do not use the proxy")
	cmd.Flags().StringVarP(&o.namespace, "namespace", "n", "krateo-system", "namespace where to install krateo runtime")
	cmd.Flags().BoolVar(&o.noCrossplane, "no-crossplane", false, "do not install crossplane")
	cmd.Flags().BoolVar(&o.openshift, "openshift", false, "true if installing Krateo on OpenShift")

	return cmd
}

const (
	crossplaneHelmIndexURL = "https://charts.crossplane.io/stable/index.yaml"
	coreModuleName         = "core.modules.krateo.io"
)

type initOpts struct {
	kubeconfig        string
	kubeconfigContext string
	bus               eventbus.Bus
	restConfig        *rest.Config
	namespace         string
	verbose           bool
	httpProxy         string
	httpsProxy        string
	noProxy           string
	noCrossplane      bool
	openshift         bool
}

func (o *initOpts) complete() (err error) {
	yml, err := ioutil.ReadFile(o.kubeconfig)
	if err != nil {
		return err
	}

	o.restConfig, err = core.RESTConfigFromBytes(yml, o.kubeconfigContext)
	if err != nil {
		return err
	}

	return nil
}

func (o *initOpts) run() error {
	ctx := context.TODO()

	if o.noCrossplane == false {
		if err := o.installCrossplane(ctx); err != nil {
			return err
		}
	}

	if err := o.installPackages(ctx); err != nil {
		return err
	}

	if err := o.createClusterRoleBindings(ctx); err != nil {
		return err
	}

	if err := o.installCoreModule(ctx); err != nil {
		return err
	}

	if err := o.promptForRequiredFields(ctx); err != nil {
		return err
	}
	return nil
}

func (o *initOpts) installCrossplane(ctx context.Context) error {
	ok, err := crossplane.Exists(ctx, crossplane.ExistOpts{
		RESTConfig: o.restConfig,
		Namespace:  o.namespace,
	})
	if err != nil {
		return err
	}
	if ok {
		return nil
	}

	idx, err := helm.IndexFromURL(crossplaneHelmIndexURL)
	if err != nil {
		return err
	}

	ver, url, err := helm.LatestVersionAndURL(idx)
	if err != nil {
		return err
	}

	o.bus.Publish(events.NewStartWaitEvent("installing crossplane %s...", ver))

	err = crossplane.Install(ctx, crossplane.InstallOpts{
		RESTConfig: o.restConfig,
		ChartURL:   url,
		Namespace:  o.namespace,
		EventBus:   o.bus,
		HttpProxy:  o.httpProxy,
		HttpsProxy: o.httpsProxy,
		NoProxy:    o.noProxy,
		Verbose:    o.verbose,
	})
	if err != nil {
		return err
	}

	o.bus.Publish(events.NewDoneEvent("crossplane %s installed", ver))

	return nil
}

func (o *initOpts) installPackages(ctx context.Context) error {
	list, err := catalog.FilterBy(catalog.ForCLI())
	if err != nil {
		return fmt.Errorf("fetching packages from catalog: %w", err)
	}

	for _, el := range list.Items {
		o.bus.Publish(events.NewStartWaitEvent("installing package %s (%s)...", el.Name, el.Version))
		err := providers.Install(ctx, providers.InstallOpts{
			RESTConfig: o.restConfig,
			Info:       &el,
			Namespace:  o.namespace,
			HttpProxy:  o.httpProxy,
			HttpsProxy: o.httpsProxy,
			NoProxy:    o.noProxy,
		})
		if err != nil {
			return fmt.Errorf("installing package '%s': %w", el.Name, err)
		}
		o.bus.Publish(events.NewDoneEvent("package %s (%s) installed", el.Name, el.Version))
		if o.verbose {
			o.bus.Publish(events.NewDebugEvent("> image: %s", el.Image))
		}
	}

	return nil
}

func (o *initOpts) createClusterRoleBindings(ctx context.Context) error {
	all, err := core.List(ctx, core.ListOpts{
		RESTConfig: o.restConfig,
		GVK: schema.GroupVersionKind{
			Version: "v1",
			Kind:    "ServiceAccount",
		},
		Namespace: o.namespace,
	})
	if err != nil {
		return err
	}

	acceptFn := func(el unstructured.Unstructured) bool {
		keep := strings.HasPrefix(el.GetName(), "provider-helm")
		keep = keep || strings.HasPrefix(el.GetName(), "provider-kubernetes")
		return keep
	}

	res, err := core.Filter(all, acceptFn)
	if err != nil {
		return err
	}

	if o.verbose {
		o.bus.Publish(events.NewDebugEvent("found [%d] service accounts", len(res)))
		for _, el := range res {
			o.bus.Publish(events.NewDebugEvent(" > %s", el.GetName()))
		}
	}

	for _, el := range res {
		idx := strings.LastIndex(el.GetName(), "-")
		name := fmt.Sprintf("%s-admin-binding", el.GetName()[0:idx])

		o.bus.Publish(events.NewStartWaitEvent("creating role bindings for %s...", name))
		err := clusterrolebindings.Create(ctx, clusterrolebindings.CreateOptions{
			RESTConfig:       o.restConfig,
			Name:             name,
			SubjectName:      el.GetName(),
			SubjectNamespace: el.GetNamespace(),
		})
		if err != nil {
			return fmt.Errorf("creating cluster role binding for '%s': %w", name, err)
		}
		o.bus.Publish(events.NewDoneEvent("role bindings '%s' created", name))
	}

	return nil
}

func (o *initOpts) installCoreModule(ctx context.Context) error {
	o.bus.Publish(events.NewStartWaitEvent("installing core module"))

	obj, err := configurations.Create(ctx, configurations.CreateOpts{
		RESTConfig: o.restConfig,
		Info: &catalog.PackageInfo{
			Name:    "krateo-module-core",
			Image:   "ghcr.io/krateoplatformops/krateo-module-core",
			Version: "latest",
		},
	})
	if err != nil {
		return err
	}

	err = configurations.WaitUntilHealtyAndInstalled(ctx, o.restConfig, obj.GetName())
	if err != nil {
		return err
	}

	o.bus.Publish(events.NewDoneEvent("core module installed"))

	return nil
}

func (o *initOpts) promptForRequiredFields(ctx context.Context) error {
	xrd, err := compositeresourcedefinitions.Get(ctx, o.restConfig, coreModuleName)
	if err != nil {
		return err
	}
	if xrd == nil {
		return nil
	}

	fields, err := compositeresourcedefinitions.GetFields(xrd, true)
	if err != nil {
		return err
	}

	res := map[string]interface{}{
		"namespace": o.namespace,
		"platform":  "kubernetes",
	}
	if o.openshift {
		res["platform"] = "openshift"
	}

	var sb strings.Builder
	for _, el := range fields {
		sb.WriteString(el.Name)
		sb.WriteRune('=')

		label := fmt.Sprintf(" <%s> %s", el.Name, el.Description)

		switch el.Type {
		case compositeresourcedefinitions.TypeBoolean:
			inp := prompt.YesNoPrompt(label, false)
			sb.WriteString(strconv.FormatBool(inp))
		default:
			inp := prompt.String(label, el.Default, true)
			sb.WriteString(inp)
		}

		sb.WriteRune(',')
	}

	if err := strvals.ParseInto(sb.String(), res); err != nil {
		return err
	}

	for k, v := range res {
		fmt.Println(k, " => ", v)
	}
	return nil
}
