package cmd

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/krateoplatformops/krateo/internal/catalog"
	"github.com/krateoplatformops/krateo/internal/claims"
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
	"github.com/krateoplatformops/krateo/internal/openshift"
	"github.com/krateoplatformops/krateo/internal/prompt"
	"github.com/krateoplatformops/krateo/internal/strvals"
	"github.com/spf13/cobra"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"

	"sigs.k8s.io/yaml"
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
	cmd.Flags().StringVar(&o.coreVersion, "core-version", "latest", "krateo core module version")
	cmd.Flags().BoolVar(&o.noCrossplane, "no-crossplane", false, "do not install crossplane")
	cmd.Flags().BoolVar(&o.openshift, "openshift", false, "true if installing Krateo on OpenShift")
	cmd.Flags().StringSliceVar(&o.values, "set", []string{}, "allows you to define values used in core module")
	cmd.Flags().MarkHidden("set")

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
	coreVersion       string
	values            []string
}

func (o *initOpts) complete() (err error) {
	yml, err := os.ReadFile(o.kubeconfig)
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
	ctx := context.Background()

	if !o.noCrossplane {
		if err := o.installCrossplane(ctx); err != nil {
			return err
		}
	}

	if err := o.createClusterRoleAndBindingForOpenshift(ctx); err != nil {
		return err
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

	vals, err := o.promptForClaims(ctx)
	if err != nil {
		return err
	}

	if err := o.applyClaims(ctx, vals); err != nil {
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

func (o *initOpts) createClusterRoleAndBindingForOpenshift(ctx context.Context) error {
	if !o.openshift {
		return nil
	}

	if err := openshift.CreateClusterRole(ctx, o.restConfig); err != nil {
		return err
	}

	return openshift.CreateClusterRoleBinding(ctx, o.restConfig, o.namespace)
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
	if len(o.coreVersion) == 0 {
		o.coreVersion = "latest"
	}

	o.bus.Publish(events.NewStartWaitEvent("installing core module"))

	obj, err := configurations.Create(ctx, configurations.CreateOpts{
		RESTConfig: o.restConfig,
		Info: &catalog.PackageInfo{
			Name:    "krateo-module-core",
			Image:   "ghcr.io/krateoplatformops/krateo-module-core",
			Version: o.coreVersion,
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

func (o *initOpts) promptForClaims(ctx context.Context) ([]string, error) {
	xrd, err := compositeresourcedefinitions.Get(ctx, o.restConfig, coreModuleName)
	if err != nil {
		return nil, err
	}
	if xrd == nil {
		return nil, nil
	}

	fields, err := compositeresourcedefinitions.GetSpecFields(xrd)
	if err != nil {
		return nil, err
	}

	res := []string{
		fmt.Sprintf("namespace=%s", o.namespace),
	}
	if o.openshift {
		res = append(res, fmt.Sprintf("platform=%s", "openshift"))
	} else {
		res = append(res, fmt.Sprintf("platform=%s", "kubernetes"))
	}

	for _, el := range fields {
		if !el.Required {
			if len(el.Default) != 0 {
				val, err := defaultFieldValue(el)
				if err != nil {
					return res, err
				}
				res = append(res, val)
			}
			continue
		}

		res = append(res, promptForFieldValue(el))
	}

	return res, nil
}

func promptForFieldValue(el compositeresourcedefinitions.Field) (val string) {
	label := fmt.Sprintf(" > %s", el.Name)

	switch el.Type {
	case compositeresourcedefinitions.TypeBoolean:
		var def bool
		if b, err := strconv.ParseBool(el.Default); err == nil {
			def = b
		}
		inp := prompt.YesNoPrompt(label, def)
		val = fmt.Sprintf("%s=%t", el.Name, inp)

	case compositeresourcedefinitions.TypeInteger:
		inp := prompt.String(label, el.Default, true)
		if i, err := strconv.Atoi(inp); err == nil {
			val = fmt.Sprintf("%s=%d", el.Name, i)
		}

	case compositeresourcedefinitions.TypeNumber:
		inp := prompt.String(label, el.Default, true)
		if f, err := strconv.ParseFloat(inp, 64); err != nil {
			val = fmt.Sprintf("%s=%f", el.Name, f)
		}
	default:
		inp := prompt.String(label, el.Default, true)
		val = fmt.Sprintf("%s=%s", el.Name, inp)
	}

	return val
}

func defaultFieldValue(el compositeresourcedefinitions.Field) (string, error) {
	switch el.Type {
	case compositeresourcedefinitions.TypeBoolean:
		val, err := strconv.ParseBool(el.Default)
		if err != nil {
			return "", err
		}
		return fmt.Sprintf("%s=%t", el.Name, val), nil

	case compositeresourcedefinitions.TypeInteger:
		val, err := strconv.Atoi(el.Default)
		if err != nil {
			return "", err
		}
		return fmt.Sprintf("%s=%d", el.Name, val), nil

	case compositeresourcedefinitions.TypeNumber:
		val, err := strconv.ParseFloat(el.Default, 64)
		if err != nil {
			return "", err
		}
		return fmt.Sprintf("%s=%f", el.Name, val), nil

	default:
		return fmt.Sprintf("%s=%s", el.Name, el.Default), nil
	}
}

func (o *initOpts) applyClaims(ctx context.Context, vals []string) error {
	o.bus.Publish(events.NewStartWaitEvent("installing core module claims ..."))

	inp := make(map[string]interface{})
	err := strvals.ParseInto(strings.Join(vals, ","), inp)
	if err != nil {
		return err
	}

	if len(o.values) > 0 {
		err := strvals.ParseInto(strings.Join(o.values, ","), inp)
		if err != nil {
			return err
		}
	}

	if o.verbose {
		//o.bus.Publish(events.NewDebugEvent(spew.Sdump(inp)))
		b, err := yaml.Marshal(inp)
		if err == nil {
			o.bus.Publish(events.NewDebugEvent(string(b)))
		}
	}

	err = claims.ApplyCoreModule(ctx, claims.ModuleOpts{
		RESTConfig: o.restConfig,
		Data:       inp,
	})
	if err != nil {
		return err
	}

	o.bus.Publish(events.NewDoneEvent("core module claims installed"))

	o.bus.Publish(events.NewStartWaitEvent("waiting for Krateo readiness ..."))
	err = claims.WaitUntilModuleCoreIsReady(ctx, o.restConfig)
	if err != nil {
		return err
	}

	o.bus.Publish(events.NewDoneEvent("Krateo is ready"))

	fmt.Printf("\nYou can now use your Krateo PlatformOps instance at the following url:\n")
	fmt.Printf("  >> https://app.%s\n\n", inp["domain"])

	return nil
}
