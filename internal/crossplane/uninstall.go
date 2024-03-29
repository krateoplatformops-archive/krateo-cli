package crossplane

import (
	"fmt"

	"github.com/krateoplatformops/krateo/internal/eventbus"
	"github.com/krateoplatformops/krateo/internal/events"
	"github.com/krateoplatformops/krateo/internal/helm"
	"k8s.io/client-go/rest"
)

type UninstallOpts struct {
	RESTConfig *rest.Config
	Namespace  string
	Verbose    bool
	EventBus   eventbus.Bus
}

func Uninstall(opts UninstallOpts) error {
	return helm.Uninstall(helm.UninstallOptions{
		RESTConfig:  opts.RESTConfig,
		Namespace:   opts.Namespace,
		ReleaseName: chartReleaseName,
		LogFn: func(format string, v ...interface{}) {
			if opts.Verbose && opts.EventBus != nil {
				msg := fmt.Sprintf(format, v...)
				opts.EventBus.Publish(events.NewDebugEvent(msg))
			}
		},
	})
}
