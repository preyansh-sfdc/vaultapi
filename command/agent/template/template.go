// Package template is responsible for rendering user supplied templates to
// disk. The Server type accepts configuration to communicate to a Vault server
// and a Vault token for authentication. Internally, the Server creates a Consul
// Template Runner which manages reading secrets from Vault and rendering
// templates to disk at configured locations
package template

import (
	"context"
	"io"
	"strings"

	ctconfig "github.com/hashicorp/consul-template/config"
	ctlogging "github.com/hashicorp/consul-template/logging"
	"github.com/hashicorp/consul-template/manager"
	"github.com/hashicorp/go-hclog"
	"github.com/hashicorp/vault/command/agent/config"
	"github.com/hashicorp/vault/sdk/helper/pointerutil"
)

// ServerConfig is a config struct for setting up the basic parts of the
// Server
type ServerConfig struct {
	Logger hclog.Logger
	// Client        *api.Client
	VaultConf     *config.Vault
	ExitAfterAuth bool

	Namespace string

	// LogLevel is needed to set the internal Consul Template Runner's log level
	// to match the log level of Vault Agent. The internal Runner creates it's own
	// logger and can't be set externally or copied from the Template Server.
	//
	// LogWriter is needed to initialize Consul Template's internal logger to use
	// the same io.Writer that Vault Agent itself is using.
	LogLevel  hclog.Level
	LogWriter io.Writer
}

// Server manages the Consul Template Runner which renders templates
type Server struct {
	// config holds the ServerConfig used to create it. It's passed along in other
	// methods
	config *ServerConfig

	// runner is the consul-template runner
	runner *manager.Runner

	// Templates holds the parsed Consul Templates
	Templates []*ctconfig.TemplateConfig

	DoneCh        chan struct{}
	logger        hclog.Logger
	exitAfterAuth bool
}

// NewServer returns a new configured server
func NewServer(conf *ServerConfig) *Server {
	ts := Server{
		DoneCh:        make(chan struct{}),
		logger:        conf.Logger,
		config:        conf,
		exitAfterAuth: conf.ExitAfterAuth,
	}
	return &ts
}

// Run kicks off the internal Consul Template runner, and listens for changes to
// the token from the AuthHandler. If Done() is called on the context, shut down
// the Runner and return
func (ts *Server) Run(ctx context.Context, incoming chan string, templates []*ctconfig.TemplateConfig) {
	latestToken := new(string)
	ts.logger.Info("starting template server")
	// defer the closing of the DoneCh
	defer func() {
		ts.logger.Info("template server stopped")
		close(ts.DoneCh)
	}()

	if incoming == nil {
		panic("incoming channel is nil")
	}

	// If there are no templates, return
	if len(templates) == 0 {
		ts.logger.Info("no templates found")
		return
	}

	// construct a consul template vault config based the agents vault
	// configuration
	var runnerConfig *ctconfig.Config
	var runnerConfigErr error
	if runnerConfig, runnerConfigErr = newRunnerConfig(ts.config, templates); runnerConfigErr != nil {
		ts.logger.Error("template server failed to generate runner config", "error", runnerConfigErr)
		return
	}

	var err error
	ts.runner, err = manager.NewRunner(runnerConfig, false)
	if err != nil {
		ts.logger.Error("template server failed to create", "error", err)
		return
	}

	for {
		select {
		case <-ctx.Done():
			ts.runner.Stop()
			return

		case token := <-incoming:
			if token != *latestToken {
				ts.logger.Info("template server received new token")
				ts.runner.Stop()
				*latestToken = token
				ctv := ctconfig.Config{
					Vault: &ctconfig.VaultConfig{
						Token: latestToken,
					},
				}
				runnerConfig = runnerConfig.Merge(&ctv)
				var runnerErr error
				ts.runner, runnerErr = manager.NewRunner(runnerConfig, false)
				if runnerErr != nil {
					ts.logger.Error("template server failed with new Vault token", "error", runnerErr)
					continue
				}
				go ts.runner.Start()
			}
		case err := <-ts.runner.ErrCh:
			ts.logger.Error("template server error", "error", err.Error())
			return
		case <-ts.runner.TemplateRenderedCh():
			if ts.exitAfterAuth {
				// if we want to exit after auth, go ahead and shut down the runner and
				// return. The deferred closing of the DoneCh will allow agent to
				// continue with closing down
				ts.runner.Stop()
				return
			}
		}
	}
}

// newRunnerConfig returns a consul-template runner configuration, setting the
// Vault and Consul configurations based on the clients configs.
func newRunnerConfig(sc *ServerConfig, templates ctconfig.TemplateConfigs) (*ctconfig.Config, error) {
	conf := ctconfig.DefaultConfig()
	conf.Templates = templates.Copy()

	// Setup the Vault config
	// Always set these to ensure nothing is picked up from the environment
	conf.Vault.RenewToken = pointerutil.BoolPtr(false)
	conf.Vault.Token = pointerutil.StringPtr("")
	conf.Vault.Address = &sc.VaultConf.Address

	if sc.Namespace != "" {
		conf.Vault.Namespace = &sc.Namespace
	}

	conf.Vault.SSL = &ctconfig.SSLConfig{
		Enabled:    pointerutil.BoolPtr(false),
		Verify:     pointerutil.BoolPtr(false),
		Cert:       pointerutil.StringPtr(""),
		Key:        pointerutil.StringPtr(""),
		CaCert:     pointerutil.StringPtr(""),
		CaPath:     pointerutil.StringPtr(""),
		ServerName: pointerutil.StringPtr(""),
	}

	if strings.HasPrefix(sc.VaultConf.Address, "https") || sc.VaultConf.CACert != "" {
		skipVerify := sc.VaultConf.TLSSkipVerify
		verify := !skipVerify
		conf.Vault.SSL = &ctconfig.SSLConfig{
			Enabled: pointerutil.BoolPtr(true),
			Verify:  &verify,
			Cert:    &sc.VaultConf.ClientCert,
			Key:     &sc.VaultConf.ClientKey,
			CaCert:  &sc.VaultConf.CACert,
			CaPath:  &sc.VaultConf.CAPath,
		}
	}

	conf.Finalize()

	// setup log level from TemplateServer config
	conf.LogLevel = logLevelToStringPtr(sc.LogLevel)

	if err := ctlogging.Setup(&ctlogging.Config{
		Name:   "template.server.runner",
		Level:  *conf.LogLevel,
		Writer: sc.LogWriter,
	}); err != nil {
		return nil, err
	}
	return conf, nil
}

// logLevelToString converts a go-hclog level to a matching, uppercase string
// value. It's used to convert Vault Agent's hclog level to a string version
// suitable for use in Consul Template's runner configuration input.
func logLevelToStringPtr(level hclog.Level) *string {
	// consul template's default level is WARN, but Vault Agent's default is INFO,
	// so we use that for the Runner's default.
	var levelStr string

	switch level {
	case hclog.Trace:
		levelStr = "TRACE"
	case hclog.Debug:
		levelStr = "DEBUG"
	case hclog.Warn:
		levelStr = "WARN"
	case hclog.Error:
		levelStr = "ERROR"
	default:
		levelStr = "INFO"
	}
	return pointerutil.StringPtr(levelStr)
}
