package main

import (
	"net"
	"net/http"
	_ "net/http/pprof"
	"os"

	"github.com/hashicorp/go-hclog"
	"github.com/hashicorp/vault/api"
	"github.com/hashicorp/vault/sdk/plugin"

	"github.com/werf/logboek"
	logboekLevel "github.com/werf/logboek/pkg/level"
	trdl "github.com/werf/trdl/server"
	"github.com/werf/trdl/server/pkg/util"
)

func main() {
	hclogOpts := &hclog.LoggerOptions{
		IncludeLocation: true,
	}

	if util.IsEnvVarTrue("VAULT_PLUGIN_SECRETS_TRDL_DEBUG") {
		hclogOpts.Level = hclog.Trace

		logboek.DefaultLogger().SetAcceptedLevel(logboekLevel.Debug)
		logboek.DefaultLogger().Streams().EnablePrefixWithTime()
	} else {
		hclogOpts.Level = hclog.Info

		logboek.DefaultLogger().SetAcceptedLevel(logboekLevel.Info)
	}

	hclog.DefaultOptions = hclogOpts

	if util.IsEnvVarTrue("VAULT_PLUGIN_SECRETS_TRDL_PPROF_ENABLE") {
		go servePprof()
	}

	apiClientMeta := &api.PluginAPIClientMeta{}
	flags := apiClientMeta.FlagSet()
	_ = flags.Parse(os.Args[1:]) // Ignore command, strictly parse flags

	tlsConfig := apiClientMeta.GetTLSConfig()
	tlsProviderFunc := api.VaultPluginTLSProvider(tlsConfig)

	if err := plugin.Serve(&plugin.ServeOpts{
		BackendFactoryFunc: trdl.Factory,
		TLSProviderFunc:    tlsProviderFunc,
	}); err != nil {
		os.Exit(1)
	}
}

func servePprof() {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		// hclog.L().Warn(fmt.Sprintf("can't serve pprof: %s", err))
		return
	}

	// hclog.L().Info(fmt.Sprintf("pprof for PID %d will be available on http://127.0.0.1:%d/debug/pprof", os.Getpid(), listener.Addr().(*net.TCPAddr).Port))
	if err := http.Serve(listener, nil); err != nil {
		// hclog.L().Warn(fmt.Sprintf("can't serve pprof: %s", err))
		return
	}
}
