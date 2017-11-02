package plugins

import (
	"common"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/hexdecteam/easegateway-types/pipelines"
	"github.com/hexdecteam/easegateway-types/plugins"
	"github.com/hexdecteam/easegateway-types/task"
	"golang.org/x/net/netutil"

	"logger"
)

type httpServerConfig struct {
	common.PluginCommonConfig
	Host             string `json:"host"`
	Port             uint16 `json:"port"` // up to 65535
	CertFile         string `json:"cert_file"`
	KeyFile          string `json:"key_file"`
	ConnKeepAlive    bool   `json:"keepalive"`
	ConnKeepAliveSec uint16 `json:"keepalive_sec"` // up to 65535
	// TODO: Adds keepalive_requests support
	MaxSimulConns uint32 `json:"max_connections"` // up to 4294967295

	certFilePath, keyFilePath string
	https                     bool
}

func httpServerConfigConstructor() plugins.Config {
	return &httpServerConfig{
		Host:             "localhost",
		Port:             10080,
		ConnKeepAlive:    true,
		ConnKeepAliveSec: 10,
		MaxSimulConns:    1024,
	}
}

func (c *httpServerConfig) Prepare(pipelineNames []string) error {
	err := c.PluginCommonConfig.Prepare(pipelineNames)
	if err != nil {
		return err
	}

	ts := strings.TrimSpace
	c.Host = ts(c.Host)
	c.CertFile = ts(c.CertFile)
	c.KeyFile = ts(c.KeyFile)

	if len(c.Host) == 0 {
		return fmt.Errorf("invalid host")
	}

	if len(c.CertFile) != 0 || len(c.KeyFile) != 0 {
		c.certFilePath = filepath.Join(common.CERT_HOME_DIR, c.CertFile)
		c.keyFilePath = filepath.Join(common.CERT_HOME_DIR, c.KeyFile)

		if s, err := os.Stat(c.certFilePath); os.IsNotExist(err) || s.IsDir() {
			return fmt.Errorf("cert file %s not found", c.CertFile)
		}

		if s, err := os.Stat(c.keyFilePath); os.IsNotExist(err) || s.IsDir() {
			return fmt.Errorf("key file %s not found", c.KeyFile)
		}

		c.https = true
	}

	if c.Port == 0 {
		return fmt.Errorf("invalid port")
	}

	if c.ConnKeepAliveSec == 0 {
		return fmt.Errorf("invalid connection keep-alive period")
	}

	if c.MaxSimulConns == 0 {
		return fmt.Errorf("invalid max simultaneous connection amount")
	}

	return nil
}

type httpServer struct {
	conf     *httpServerConfig
	addr     string
	listener net.Listener
	server   *http.Server
	mux      plugins.HTTPMux
	closed   bool
}

func httpServerConstructor(conf plugins.Config) (plugins.Plugin, error) {
	c, ok := conf.(*httpServerConfig)
	if !ok {
		return nil, fmt.Errorf("config type want *httpServerConfig got %T", conf)
	}

	h := &httpServer{
		conf: c,
	}

	h.addr = fmt.Sprintf("%s:%d", c.Host, c.Port)

	ln, err := net.Listen("tcp", h.addr)
	if err != nil {
		return nil, err
	}

	h.listener = netutil.LimitListener(&tcpKeepAliveListener{
		connKeepAlive:    c.ConnKeepAlive,
		connKeepAliveSec: c.ConnKeepAliveSec,
		tcpListener:      ln.(*net.TCPListener),
	}, int(c.MaxSimulConns))

	h.mux = newMux()

	h.server = &http.Server{
		Handler: h.mux,
	}

	h.server.SetKeepAlivesEnabled(c.ConnKeepAlive)
	if c.ConnKeepAlive {
		h.server.IdleTimeout = time.Duration(c.ConnKeepAliveSec) * time.Second
	}

	done := make(chan error)
	defer close(done)

	server_startup_notifier := func(e error) {
		defer func() {
			// server will be shutdown during close, ignore safely
			recover()
		}()
		done <- e
	}

	if c.https {
		logger.Debugf("[https server %s is starting at %s]", c.Name, h.addr)

		go func() {
			err := h.server.ServeTLS(ln, c.certFilePath, c.keyFilePath)
			if !h.closed && err != nil {
				logger.Errorf("[https server listens %s failed: %v]", h.addr, err)
			}
			server_startup_notifier(err)
		}()
	} else {
		logger.Debugf("[http server %s is starting at %s]", c.Name, h.addr)

		go func() {
			err := h.server.Serve(ln)
			if !h.closed && err != nil {
				logger.Errorf("[http server listens %s failed: %v]", h.addr, err)
			}
			server_startup_notifier(err)
		}()
	}

	select {
	case err = <-done:
	default:
	}

	if err != nil {
		h.listener.Close()
		h.closed = true
		return nil, err
	}

	return h, nil
}

func (h *httpServer) Prepare(ctx pipelines.PipelineContext) {
	pipeline_rtable := getPipelineRouteTable(ctx, h.Name())
	if pipeline_rtable != nil {
		h.mux.AddFuncs(ctx.PipelineName(), pipeline_rtable)
	}

	storeHTTPServerMux(ctx, h.Name(), h.mux)
	storeHTTPServerGoneNotifier(ctx, h.Name(), make(chan struct{}))
}

func (h *httpServer) Run(ctx pipelines.PipelineContext, t task.Task) (task.Task, error) {
	// Nothing to do
	return t, nil
}

func (h *httpServer) Name() string {
	return h.conf.PluginName()
}

func (h *httpServer) CleanUp(ctx pipelines.PipelineContext) {
	mux := getHTTPServerMux(ctx, h.Name(), true)
	if mux == nil {
		// doesn't make sense, defensive
		return
	}

	pipeline_rtable := mux.DeleteFuncs(ctx.PipelineName())
	if pipeline_rtable != nil {
		storePipelineRouteTable(ctx, h.Name(), pipeline_rtable)
	}

	notifier := getHTTPServerGoneNotifier(ctx, h.Name(), true)
	if notifier != nil {
		close(notifier)
	}
}

func (h *httpServer) Close() {
	h.closed = true

	err := h.server.Close()
	if err != nil {
		logger.Errorf("[shut server listens at %s down failed: %v]", h.addr, err)
	} else {
		logger.Debugf("[server listens at %s is shut down]", h.addr)
	}
}

////

type tcpKeepAliveListener struct {
	connKeepAlive    bool
	connKeepAliveSec uint16
	tcpListener      *net.TCPListener
}

func (ln tcpKeepAliveListener) Accept() (c net.Conn, err error) {
	tc, err := ln.tcpListener.AcceptTCP()
	if err != nil {
		return
	}

	tc.SetKeepAlive(ln.connKeepAlive)
	if ln.connKeepAlive {
		tc.SetKeepAlivePeriod(time.Duration(ln.connKeepAliveSec) * time.Second)
	}

	return tc, nil
}

func (ln tcpKeepAliveListener) Close() error {
	return ln.tcpListener.Close()
}

func (ln tcpKeepAliveListener) Addr() net.Addr {
	return ln.tcpListener.Addr()
}

////

func storeHTTPServerMux(ctx pipelines.PipelineContext, pluginName string, mux plugins.HTTPMux) error {
	bucket := ctx.DataBucket(pluginName, pipelines.DATA_BUCKET_FOR_ALL_PLUGIN_INSTANCE)
	_, err := bucket.BindData(plugins.HTTP_SERVER_MUX_BUCKET_KEY, mux)
	if err != nil {
		logger.Warnf("[BUG: store the mux of http server %s for pipeline %s failed, "+
			"ignored to provide mux: %v]", pluginName, ctx.PipelineName(), err)
		return err
	}

	return nil
}

func getHTTPServerMux(ctx pipelines.PipelineContext, pluginName string, required bool) plugins.HTTPMux {
	bucket := ctx.DataBucket(pluginName, pipelines.DATA_BUCKET_FOR_ALL_PLUGIN_INSTANCE)
	mux := bucket.QueryData(plugins.HTTP_SERVER_MUX_BUCKET_KEY)

	ret, ok := mux.(plugins.HTTPMux)
	if !ok && required {
		logger.Errorf("[the mux of http server %s for pipeline %s is invalid]",
			pluginName, ctx.PipelineName())
		return nil
	}

	return ret
}

func storePipelineRouteTable(ctx pipelines.PipelineContext, pluginName string,
	pipeline_rtable map[string]map[string]*plugins.HTTPMuxEntry) error {

	bucket := ctx.DataBucket(pluginName, pipelines.DATA_BUCKET_FOR_ALL_PLUGIN_INSTANCE)
	_, err := bucket.BindData(plugins.HTTP_SERVER_PIPELINE_ROUTE_TABLE_BUCKET_KEY, pipeline_rtable)
	if err != nil {
		logger.Errorf("[BUG: store the route table of pipeline %s for http server %s failed: %v]",
			ctx.PipelineName(), pluginName, err)
		return err
	}

	return nil
}

func getPipelineRouteTable(ctx pipelines.PipelineContext,
	pluginName string) map[string]map[string]*plugins.HTTPMuxEntry {

	bucket := ctx.DataBucket(pluginName, pipelines.DATA_BUCKET_FOR_ALL_PLUGIN_INSTANCE)
	pipeline_rtable := bucket.QueryData(plugins.HTTP_SERVER_PIPELINE_ROUTE_TABLE_BUCKET_KEY)

	if pipeline_rtable == nil {
		return nil
	}

	ret, ok := pipeline_rtable.(map[string]map[string]*plugins.HTTPMuxEntry)
	if !ok {
		logger.Errorf("[the route table of pipeline %s for http server %s is invalid]",
			ctx.PipelineName(), pluginName)
		return nil
	}

	return ret
}

func storeHTTPServerGoneNotifier(ctx pipelines.PipelineContext, pluginName string, notifier chan struct{}) error {
	bucket := ctx.DataBucket(pluginName, pipelines.DATA_BUCKET_FOR_ALL_PLUGIN_INSTANCE)
	_, err := bucket.BindData(plugins.HTTP_SERVER_GONE_NOTIFIER_BUCKET_KEY, notifier)
	if err != nil {
		logger.Warnf("[BUG: store the close notifier of http server %s for pipeline %s failed, "+
			"ignored to provide close notifier: %v]", pluginName, ctx.PipelineName(), err)
		return err
	}

	return nil
}

func getHTTPServerGoneNotifier(ctx pipelines.PipelineContext, pluginName string, required bool) chan struct{} {
	bucket := ctx.DataBucket(pluginName, pipelines.DATA_BUCKET_FOR_ALL_PLUGIN_INSTANCE)
	notifier := bucket.QueryData(plugins.HTTP_SERVER_GONE_NOTIFIER_BUCKET_KEY)

	ret, ok := notifier.(chan struct{})
	if !ok && required {
		logger.Errorf("[the close notifier of http server %s for pipeline %s is invalid]",
			pluginName, ctx.PipelineName())
		return nil
	}

	return ret
}
