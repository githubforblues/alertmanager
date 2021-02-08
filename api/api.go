// Copyright 2019 Prometheus Team
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
// http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package api

import (
	"errors"
	"fmt"
	"net/http"
	"runtime"
	"time"

	apiv1 "github.com/prometheus/alertmanager/api/v1"
	apiv2 "github.com/prometheus/alertmanager/api/v2"
	"github.com/prometheus/alertmanager/cluster"
	"github.com/prometheus/alertmanager/config"
	"github.com/prometheus/alertmanager/dispatch"
	"github.com/prometheus/alertmanager/provider"
	"github.com/prometheus/alertmanager/silence"
	"github.com/prometheus/alertmanager/types"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/common/model"
	"github.com/prometheus/common/route"

	"github.com/go-kit/kit/log"
)

// API represents all APIs of Alertmanager.
type API struct {
	v1                       *apiv1.API
	v2                       *apiv2.API
	requestsInFlight         prometheus.Gauge			//属性含义：飞行中的请求?
	concurrencyLimitExceeded prometheus.Counter			//属性含义：超出并发限制
	timeout                  time.Duration
	inFlightSem              chan struct{}				//属性含义：一个通道
}

// Options for the creation of an API object. Alerts, Silences, and StatusFunc
// are mandatory to set. The zero value for everything else is a safe default.
//创建API对象时的参数，Alerts, Silences, and StatusFunc必须设置参数，参数全设置为0是安全的默认值
type Options struct {
	// Alerts to be used by the API. Mandatory.
	Alerts provider.Alerts
	// Silences to be used by the API. Mandatory.
	Silences *silence.Silences
	// StatusFunc is used be the API to retrieve the AlertStatus of an
	// alert. Mandatory.
	StatusFunc func(model.Fingerprint) types.AlertStatus
	// Peer from the gossip cluster. If nil, no clustering will be used.
	Peer *cluster.Peer
	// Timeout for all HTTP connections. The zero value (and negative
	// values) result in no timeout.
	Timeout time.Duration
	// Concurrency limit for GET requests. The zero value (and negative
	// values) result in a limit of GOMAXPROCS or 8, whichever is
	// larger. Status code 503 is served for GET requests that would exceed
	// the concurrency limit.
	Concurrency int
	// Logger is used for logging, if nil, no logging will happen.
	Logger log.Logger
	// Registry is used to register Prometheus metrics. If nil, no metrics
	// registration will happen.
	Registry prometheus.Registerer
	// GroupFunc returns a list of alert groups. The alerts are grouped
	// according to the current active configuration. Alerts returned are
	// filtered by the arguments provided to the function.
	GroupFunc func(func(*dispatch.Route) bool, func(*types.Alert, time.Time) bool) (dispatch.AlertGroups, map[model.Fingerprint][]string)
}

func (o Options) validate() error {
	if o.Alerts == nil {
		return errors.New("mandatory field Alerts not set")
	}
	if o.Silences == nil {
		return errors.New("mandatory field Silences not set")
	}
	if o.StatusFunc == nil {
		return errors.New("mandatory field StatusFunc not set")
	}
	if o.GroupFunc == nil {
		return errors.New("mandatory field GroupFunc not set")
	}
	return nil
}

// New creates a new API object combining all API versions. Note that an Update
// call is also needed to get the APIs into an operational state.
//新建API对象。请注意，还需要进行Update调用才能使API进入运行状态。
func New(opts Options) (*API, error) {
	if err := opts.validate(); err != nil {
		return nil, fmt.Errorf("invalid API options: %s", err)
	}
	l := opts.Logger
	if l == nil {
		//如果opts对象没有携带logger对象作为属性，则此处新建一个logger来使用
		l = log.NewNopLogger()
	}
	concurrency := opts.Concurrency
	if concurrency < 1 {
		concurrency = runtime.GOMAXPROCS(0)
		if concurrency < 8 {
			concurrency = 8
		}
	}

	//创建api对象时，将opts对象的属性值传递过去
	v1 := apiv1.New(
		opts.Alerts,
		opts.Silences,
		opts.StatusFunc,
		opts.Peer,
		log.With(l, "version", "v1"),
		opts.Registry,
	)

	v2, err := apiv2.NewAPI(
		opts.Alerts,
		opts.GroupFunc,
		opts.StatusFunc,
		opts.Silences,
		opts.Peer,
		log.With(l, "version", "v2"),
		opts.Registry,
	)

	if err != nil {
		return nil, err
	}

	// TODO(beorn7): For now, this hardcodes the method="get" label. Other
	// methods should get the same instrumentation.
	//创建am api相关的指标，此处写死了method=get
	requestsInFlight := prometheus.NewGauge(prometheus.GaugeOpts{
		Name:        "alertmanager_http_requests_in_flight",
		Help:        "Current number of HTTP requests being processed.",
		ConstLabels: prometheus.Labels{"method": "get"},
	})
	concurrencyLimitExceeded := prometheus.NewCounter(prometheus.CounterOpts{
		Name:        "alertmanager_http_concurrency_limit_exceeded_total",
		Help:        "Total number of times an HTTP request failed because the concurrency limit was reached.",
		ConstLabels: prometheus.Labels{"method": "get"},
	})
	//这个Registry是用来将指标注册到prometheus_client的，使用过prometheus_client就知道
	if opts.Registry != nil {
		if err := opts.Registry.Register(requestsInFlight); err != nil {
			return nil, err
		}
		if err := opts.Registry.Register(concurrencyLimitExceeded); err != nil {
			return nil, err
		}
	}

	return &API{
		v1:                       v1,
		v2:                       v2,
		requestsInFlight:         requestsInFlight,
		concurrencyLimitExceeded: concurrencyLimitExceeded,
		timeout:                  opts.Timeout,
		inFlightSem:              make(chan struct{}, concurrency),			//并发数就是这个通道的容量
	}, nil
}

// Register all APIs. It registers APIv1 with the provided router directly. As
// APIv2 works on the http.Handler level, this method also creates a new
// http.ServeMux and then uses it to register both the provided router (to
// handle "/") and APIv2 (to handle "<routePrefix>/api/v2"). The method returns
// the newly created http.ServeMux. If a timeout has been set on construction of
// API, it is enforced for all HTTP request going through this mux. The same is
// true for the concurrency limit, with the exception that it is only applied to
// GET requests.
//向路由器注册所有的API。其中v1版会直接全部注册
//当APIv2在http.Handler级别上工作时，此方法还将创建一个新的http.ServeMux，然后使用它注册提供的路由器（以处理"/"）和APIv2（以处理"<routePrefix>/api/v2"），并将创建的http.ServeMux返回
//如果在构建API时设置了超时，则对通过此多路复用器的所有HTTP请求强制执行超时。并发限制也是如此，只是它仅适用于GET请求
func (api *API) Register(r *route.Router, routePrefix string) *http.ServeMux {
	//v1直接注册
	api.v1.Register(r.WithPrefix("/api/v1"))

	//创建一个新的mux并先注册"/""
	mux := http.NewServeMux()
	mux.Handle("/", api.limitHandler(r))

	//传参routePrefix表示这里注册的所有URI是基于哪个外层URI的。比如routePrefix是/test，这里再注册/api，那么实际URI就是/test/api
	//由于上面创建了新的mux，没有使用传参进来的r，所以需要获取一下routePrefix，给新的mux使用
	apiPrefix := ""
	if routePrefix != "/" {
		apiPrefix = routePrefix
	}
	// TODO(beorn7): HTTP instrumentation is only in place for Router. Since
	// /api/v2 works on the Handler level, it is currently not instrumented
	// at all (with the exception of requestsInFlight, which is handled in
	// limitHandler below).
	mux.Handle(
		apiPrefix+"/api/v2/",
		api.limitHandler(http.StripPrefix(apiPrefix+"/api/v2", api.v2.Handler)),
	)

	return mux
}

// Update config and resolve timeout of each API. APIv2 also needs
// setAlertStatus to be updated.
//更新api的配置和解析超时，这样api才能工作
func (api *API) Update(cfg *config.Config, setAlertStatus func(model.LabelSet)) {
	api.v1.Update(cfg)
	api.v2.Update(cfg, setAlertStatus)
}

//http请求处理并发限制的实现，会返回一个带有并发限制功能的http.Handler
func (api *API) limitHandler(h http.Handler) http.Handler {
	concLimiter := http.HandlerFunc(func(rsp http.ResponseWriter, req *http.Request) {
		if req.Method == http.MethodGet { // Only limit concurrency of GETs.
			select {
			case api.inFlightSem <- struct{}{}: // All good, carry on.
				api.requestsInFlight.Inc()
				defer func() {
					<-api.inFlightSem
					api.requestsInFlight.Dec()
				}()
			default:
				api.concurrencyLimitExceeded.Inc()
				http.Error(rsp, fmt.Sprintf(
					"Limit of concurrent GET requests reached (%d), try again later.\n", cap(api.inFlightSem),
				), http.StatusServiceUnavailable)
				return
			}
		}
		h.ServeHTTP(rsp, req)
	})
	if api.timeout <= 0 {
		return concLimiter
	}
	return http.TimeoutHandler(concLimiter, api.timeout, fmt.Sprintf(
		"Exceeded configured timeout of %v.\n", api.timeout,
	))
}
