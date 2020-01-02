package httpserver

import (
	"github.com/megaease/easegateway/pkg/util/httpheader"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/megaease/easegateway/pkg/context"
	"github.com/megaease/easegateway/pkg/logger"
	"github.com/megaease/easegateway/pkg/scheduler"
	"github.com/megaease/easegateway/pkg/util/httpstat"
	"github.com/megaease/easegateway/pkg/util/ipfilter"
	"github.com/megaease/easegateway/pkg/util/stringtool"
	"github.com/megaease/easegateway/pkg/util/topn"
)

type (
	mux struct {
		spec     *Spec
		handlers *sync.Map
		httpStat *httpstat.HTTPStat
		topN     *topn.TopN

		rules atomic.Value // *muxRules
	}

	muxRules struct {
		cache *cache

		ipFilter     *ipfilter.IPFilter
		ipFilterChan *ipfilter.IPFilters

		rules []*muxRule
	}

	muxRule struct {
		ipFilter      *ipfilter.IPFilter
		ipFilterChain *ipfilter.IPFilters

		host       string
		hostRegexp string
		hostRE     *regexp.Regexp
		paths      []*muxPath
	}

	muxPath struct {
		ipFilter      *ipfilter.IPFilter
		ipFilterChain *ipfilter.IPFilters

		path       string
		pathPrefix string
		pathRegexp string
		pathRE     *regexp.Regexp
		methods    []string
		backend    string
		headers    []*Header
	}
)

// newIPFilterChain returns nil if the number of final filters is zero.
func newIPFilterChain(parentIPFilters *ipfilter.IPFilters, childSpec *ipfilter.Spec) *ipfilter.IPFilters {
	var ipFilters *ipfilter.IPFilters
	if parentIPFilters != nil {
		ipFilters = ipfilter.NewIPfilters(parentIPFilters.Filters()...)
	} else {
		ipFilters = ipfilter.NewIPfilters()
	}

	if childSpec != nil {
		ipFilters.Append(ipfilter.New(childSpec))
	}

	if len(ipFilters.Filters()) == 0 {
		return nil
	}

	return ipFilters
}

func newIPFilter(spec *ipfilter.Spec) *ipfilter.IPFilter {
	if spec == nil {
		return nil
	}

	return ipfilter.New(spec)
}

func (mr *muxRules) pass(ctx context.HTTPContext) bool {
	if mr.ipFilter == nil {
		return true
	}

	return mr.ipFilter.AllowHTTPContext(ctx)
}

func (mr *muxRules) getCacheItem(ctx context.HTTPContext) *cacheItem {
	if mr.cache == nil {
		return nil
	}

	r := ctx.Request()
	key := stringtool.Cat(r.Method(), r.Path())
	return mr.cache.get(key)
}

func (mr *muxRules) putCacheItem(ctx context.HTTPContext, ci *cacheItem) {
	if mr.cache == nil {
		return
	}

	r := ctx.Request()
	key := stringtool.Cat(r.Method(), r.Path())
	if !ci.cached {
		ci.cached = true
		// NOTE: It's fine to cover the existed item because of conccurently updating cache.
		mr.cache.put(key, ci)
	}
}

func newMuxRule(parentIPFilters *ipfilter.IPFilters, rule *Rule, paths []*muxPath) *muxRule {
	var hostRE *regexp.Regexp

	if rule.HostRegexp != "" {
		var err error
		hostRE, err = regexp.Compile(rule.HostRegexp)
		// defensive programming
		if err != nil {
			logger.Errorf("BUG: compile %s failed: %v",
				rule.HostRegexp, err)
		}
	}

	return &muxRule{
		ipFilter:      newIPFilter(rule.IPFilter),
		ipFilterChain: newIPFilterChain(parentIPFilters, rule.IPFilter),

		host:       rule.Host,
		hostRegexp: rule.HostRegexp,
		hostRE:     hostRE,
		paths:      paths,
	}
}

func (mr *muxRule) pass(ctx context.HTTPContext) bool {
	if mr.ipFilter == nil {
		return true
	}

	return mr.ipFilter.AllowHTTPContext(ctx)
}

func (mr *muxRule) match(ctx context.HTTPContext) bool {
	r := ctx.Request()

	if mr.host == "" && mr.hostRE == nil {
		return true
	}

	if mr.host != "" && mr.host == r.Host() {
		return true
	}
	if mr.hostRE != nil && mr.hostRE.MatchString(r.Host()) {
		return true
	}

	return false
}

func newMuxPath(parentIPFilters *ipfilter.IPFilters, path *Path) *muxPath {
	var pathRE *regexp.Regexp
	if path.PathRegexp != "" {
		var err error
		pathRE, err = regexp.Compile(path.PathRegexp)
		// defensive programming
		if err != nil {
			logger.Errorf("BUG: compile %s failed: %v",
				path.PathRegexp, err)
		}
	}

	for _, p := range path.Headers {
		p.initHeaderRoute()
	}

	return &muxPath{
		ipFilter:      newIPFilter(path.IPFilter),
		ipFilterChain: newIPFilterChain(parentIPFilters, path.IPFilter),

		path:       path.Path,
		pathPrefix: path.PathPrefix,
		pathRegexp: path.PathRegexp,
		pathRE:     pathRE,
		methods:    path.Methods,
		backend:    path.Backend,
		headers:    path.Headers,
	}
}

func (mp *muxPath) pass(ctx context.HTTPContext) bool {
	if mp.ipFilter == nil {
		return true
	}

	return mp.ipFilter.AllowHTTPContext(ctx)
}

func (mp *muxPath) matchPath(ctx context.HTTPContext) bool {
	r := ctx.Request()

	if mp.path == "" && mp.pathPrefix == "" && mp.pathRE == nil {
		return true
	}

	if mp.path != "" && mp.path == r.Path() {
		return true
	}
	if mp.pathPrefix != "" && strings.HasPrefix(r.Path(), mp.pathPrefix) {
		return true
	}
	if mp.pathRE != nil {
		return mp.pathRE.MatchString(r.Path())
	}

	return false
}

func (mp *muxPath) matchMethod(ctx context.HTTPContext) bool {
	if len(mp.methods) == 0 {
		return true
	}

	return stringtool.StrInSlice(ctx.Request().Method(), mp.methods)
}

func (mp *muxPath) matchHeaders(ctx context.HTTPContext) (ci *cacheItem, ok bool) {
	for _, h := range mp.headers {
		v := ctx.Request().Header().Get(h.Key)
		if stringtool.StrInSlice(v, h.Values) {
			ci = &cacheItem{ipFilterChan: mp.ipFilterChain, backend: h.Backend}
			return ci, true
		}

		if h.Regexp != "" && h.headerRE.MatchString(v) {
			ci = &cacheItem{ipFilterChan: mp.ipFilterChain, backend: h.Backend}
			return ci, true
		}
	}

	return nil, false
}

func newMux(handlers *sync.Map, httpStat *httpstat.HTTPStat, topN *topn.TopN) *mux {
	m := &mux{
		handlers: handlers,
		httpStat: httpStat,
		topN:     topN,
	}

	m.rules.Store(&muxRules{})

	return m
}

func (m *mux) reloadRules(spec *Spec) {
	m.spec = spec

	rules := &muxRules{
		ipFilter:     newIPFilter(spec.IPFilter),
		ipFilterChan: newIPFilterChain(nil, spec.IPFilter),
		rules:        make([]*muxRule, len(spec.Rules)),
	}

	if spec.CacheSize > 0 {
		rules.cache = newCache(spec.CacheSize)
	}

	var ipFilters []*ipfilter.IPFilter
	if spec.IPFilter != nil {
		ipFilters = append(ipFilters, ipfilter.New(spec.IPFilter))
	}

	for i := 0; i < len(rules.rules); i++ {
		specRule := spec.Rules[i]

		ruleIPFilterChain := newIPFilterChain(rules.ipFilterChan, specRule.IPFilter)

		paths := make([]*muxPath, len(specRule.Paths))
		for j := 0; j < len(paths); j++ {
			paths[j] = newMuxPath(ruleIPFilterChain, &specRule.Paths[j])
		}

		// NOTE: Given the parent ipFilters not its own.
		rules.rules[i] = newMuxRule(rules.ipFilterChan, &specRule, paths)
	}

	m.rules.Store(rules)
}

func (m *mux) ServeHTTP(stdw http.ResponseWriter, stdr *http.Request) {
	rules := m.rules.Load().(*muxRules)

	ctx := context.New(stdw, stdr)
	defer ctx.Finish()
	ctx.OnFinish(func() {
		m.httpStat.Stat(ctx.StatMetric())
		m.topN.Stat(ctx)
	})

	ci := rules.getCacheItem(ctx)
	if ci != nil {
		m.handleRequestWithCache(rules, ctx, ci)
		return
	}

	if !rules.pass(ctx) {
		m.handleIPNotAllow(ctx)
		return
	}

	for _, host := range rules.rules {
		if !host.match(ctx) {
			continue
		}

		if !host.pass(ctx) {
			m.handleIPNotAllow(ctx)
			return
		}

		for _, path := range host.paths {
			if !path.matchPath(ctx) {
				continue
			}

			if !path.matchMethod(ctx) {
				ci = &cacheItem{ipFilterChan: path.ipFilterChain, methodNotAllowed: true}
				rules.putCacheItem(ctx, ci)
				m.handleRequestWithCache(rules, ctx, ci)
				return
			}

			if !path.pass(ctx) {
				m.handleIPNotAllow(ctx)
				return
			}

			ci, ok := path.matchHeaders(ctx)
			if ok {
				// NOTE: must not cache the route by header
				m.handleRequestWithCache(rules, ctx, ci)
				return
			}

			ci = &cacheItem{ipFilterChan: path.ipFilterChain, backend: path.backend}
			rules.putCacheItem(ctx, ci)
			m.handleRequestWithCache(rules, ctx, ci)
			return
		}
	}

	ci = &cacheItem{ipFilterChan: rules.ipFilterChan, notFound: true}
	rules.putCacheItem(ctx, ci)
	m.handleRequestWithCache(rules, ctx, ci)
}

func (m *mux) handleIPNotAllow(ctx context.HTTPContext) {
	ctx.AddTag(stringtool.Cat("ip ", ctx.Request().RealIP(), " not allow"))
	ctx.Response().SetStatusCode(http.StatusForbidden)
}

func (m *mux) handleRequestWithCache(rules *muxRules, ctx context.HTTPContext, ci *cacheItem) {
	if ci.ipFilterChan != nil {
		if !ci.ipFilterChan.AllowHTTPContext(ctx) {
			m.handleIPNotAllow(ctx)
			return
		}
	}

	switch {
	case ci.notFound:
		ctx.Response().SetStatusCode(http.StatusNotFound)
	case ci.methodNotAllowed:
		ctx.Response().SetStatusCode(http.StatusMethodNotAllowed)
	case ci.backend != "":
		rawHandler, exists := m.handlers.Load(ci.backend)
		if !exists {
			ctx.AddTag(stringtool.Cat("backend ", ci.backend, " not found"))
			ctx.Response().SetStatusCode(http.StatusServiceUnavailable)
			return
		}

		handler, ok := rawHandler.(scheduler.HTTPHandler)
		if !ok {
			ctx.AddTag(stringtool.Cat("BUG: backend ", ci.backend, " is not a http handler"))
			ctx.Response().SetStatusCode(http.StatusServiceUnavailable)
			return
		}

		if m.spec.XForwardedFor {
			m.appendXForwardedFor(ctx)
		}

		handler.Handle(ctx)
	}
}

func (m *mux) appendXForwardedFor(ctx context.HTTPContext) {
	v := ctx.Request().Header().Get(httpheader.KeyXForwardedFor)
	ip := ctx.Request().RealIP()

	if v == "" {
		ctx.Request().Header().Add(httpheader.KeyXForwardedFor, ip)
		return
	}

	if !strings.Contains(v, ip) {
		v = stringtool.Cat(v, ",", ip)
		ctx.Request().Header().Set(httpheader.KeyXForwardedFor, v)
	}
}
