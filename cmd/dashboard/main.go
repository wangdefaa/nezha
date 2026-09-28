package main

import (
	"context"
	"embed"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"runtime/debug"
	"strings"
	"time"
	_ "time/tzdata"

	"github.com/ory/graceful"
	"golang.org/x/crypto/bcrypt"

	"github.com/nezhahq/nezha/cmd/dashboard/controller"
	"github.com/nezhahq/nezha/cmd/dashboard/rpc"
	"github.com/nezhahq/nezha/model"
	"github.com/nezhahq/nezha/pkg/idcodec"
	"github.com/nezhahq/nezha/pkg/utils"
	"github.com/nezhahq/nezha/proto"
	"github.com/nezhahq/nezha/service/singleton"
)

type DashboardCliParam struct {
	Version          bool
	ConfigFile       string
	DatabaseLocation string
}

var (
	dashboardCliParam DashboardCliParam
	//go:embed *-dist
	frontendDist embed.FS
)

// initialAdminPasswordEnv 首次安装时指定管理员初始口令的环境变量；未设置则随机生成。
const initialAdminPasswordEnv = "NZ_INIT_ADMIN_PASSWORD" // #nosec G101 -- 环境变量名，不是口令本身

// ensureInitialAdmin 库中没有用户时创建 admin。旧实现口令固定为 admin，首次部署暴露公网即可被直接接管；
// 现改为取环境变量或随机生成，随机口令只在本次启动日志打印一次。
func ensureInitialAdmin() error {
	var usersCount int64
	if err := singleton.DB.Model(&model.User{}).Count(&usersCount).Error; err != nil || usersCount > 0 {
		return err
	}
	password, generated, err := initialAdminPassword()
	if err != nil {
		return err
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		return err
	}
	if err := singleton.DB.Create(&model.User{Username: "admin", Password: string(hash)}).Error; err != nil {
		return err
	}
	if generated {
		log.Printf("NEZHA>> 已创建初始管理员 admin，随机密码：%s（仅本次显示，请登录后立即修改）", password)
	}
	return nil
}

func initialAdminPassword() (password string, generated bool, err error) {
	if p := os.Getenv(initialAdminPasswordEnv); p != "" {
		return p, false, nil
	}
	password, err = utils.GenerateRandomString(20)
	return password, true, err
}

func initSystem(bus chan<- *model.Service) error {
	if err := ensureInitialAdmin(); err != nil {
		return err
	}

	if err := singleton.LoadSingleton(bus); err != nil {
		return err
	}

	if _, err := singleton.CronShared.AddFunc("0 30 3 * * *", singleton.CleanMonitorHistory); err != nil {
		return err
	}

	if _, err := singleton.CronShared.AddFunc("0 0 * * * *", func() { singleton.RecordTransferHourlyUsage() }); err != nil {
		return err
	}

	if err := singleton.StartJWTSessionGC(); err != nil {
		return err
	}
	return nil
}

func initIDCodec() error {
	return idcodec.Init([]byte(singleton.Conf.JWTSecretKey))
}

// @title           Nezha Monitoring API
// @version         1.0
// @description     Nezha Monitoring API
// @termsOfService  http://nezhahq.github.io

// @contact.name   API Support
// @contact.url    http://nezhahq.github.io
// @contact.email  hi@nai.ba

// @license.name  Apache 2.0
// @license.url   http://www.apache.org/licenses/LICENSE-2.0.html

// @host      localhost:8008
// @BasePath  /api/v1

// @securityDefinitions.apikey  BearerAuth
// @in header
// @name Authorization
// @description JWT session token. Browser/UI flow. Format: `Bearer <jwt>` or cookie `nz-jwt`.

// @securityDefinitions.apikey  APITokenAuth
// @in header
// @name Authorization
// @description Personal Access Token (PAT). Programmatic/CI/LLM flow. Format: `Bearer nzp_<secret>`.
// @description Each endpoint enforces a specific scope; see the `controller` package godoc for the authoritative scope table.

// @externalDocs.description  OpenAPI
// @externalDocs.url          https://swagger.io/resources/open-api/
func main() {
	parseCliParams()

	serviceSentinelDispatchBus := make(chan *model.Service)
	if err := initDashboard(serviceSentinelDispatchBus); err != nil {
		log.Fatal(err)
	}

	l, err := net.Listen("tcp", fmt.Sprintf("%s:%d", singleton.Conf.ListenHost, singleton.Conf.ListenPort))
	if err != nil {
		log.Fatal(err)
	}

	singleton.CleanMonitorHistory()
	rpc.DispatchKeepalive()
	go rpc.DispatchTask(serviceSentinelDispatchBus)
	go singleton.AlertSentinelStart()

	grpcHandler := rpc.ServeRPC()
	httpHandler := controller.ServeWeb(frontendDist)
	controller.InitUpgrader()

	httpServer, httpsServer := newMuxServers(newHTTPandGRPCMux(httpHandler, grpcHandler))
	serveUntilSignal(l, httpServer, httpsServer)
}

// parseCliParams 解析命令行参数；-v 打印版本号后直接退出。
func parseCliParams() {
	flag.BoolVar(&dashboardCliParam.Version, "v", false, "查看当前版本号")
	flag.StringVar(&dashboardCliParam.ConfigFile, "c", "data/config.yaml", "配置文件路径")
	flag.StringVar(&dashboardCliParam.DatabaseLocation, "db", "data/sqlite.db", "Sqlite3数据库文件路径")
	flag.Parse()

	if dashboardCliParam.Version {
		fmt.Println(singleton.Version)
		os.Exit(0)
	}
}

// initDashboard 按依赖顺序完成启动初始化，任一步失败即短路返回。
func initDashboard(bus chan<- *model.Service) error {
	return utils.FirstError(singleton.InitFrontendTemplates,
		func() error { return singleton.InitConfigFromPath(dashboardCliParam.ConfigFile) },
		initIDCodec,
		singleton.InitTimezoneAndCache,
		applyMemoryLimit,
		func() error { return singleton.InitDBFromPath(dashboardCliParam.DatabaseLocation) },
		singleton.InitTSDB,
		func() error { return initSystem(bus) })
}

// applyMemoryLimit 按配置设置 Go 运行时软内存上限（0 表示不限制）。
func applyMemoryLimit() error {
	if mb := singleton.Conf.Memory.GoMemLimitMB; mb > 0 {
		debug.SetMemoryLimit(mb * 1024 * 1024)
		log.Printf("NEZHA>> Go memory limit set to %d MB", mb)
	}
	return nil
}

// newMuxServers 构造单端口复用 HTTP/1 与 h2c(gRPC) 的服务器；配置了 HTTPS 端口时另建 TLS 服务器，否则第二个返回值为 nil。
func newMuxServers(handler http.Handler) (*http.Server, *http.Server) {
	httpServer := &http.Server{
		Handler:           handler,
		ReadHeaderTimeout: time.Second * 5,
	}
	httpServer.Protocols = new(http.Protocols)
	httpServer.Protocols.SetHTTP1(true)
	httpServer.Protocols.SetUnencryptedHTTP2(true)

	if singleton.Conf.HTTPS.ListenPort == 0 {
		return httpServer, nil
	}
	httpsServer := &http.Server{
		Addr:              fmt.Sprintf("%s:%d", singleton.Conf.ListenHost, singleton.Conf.HTTPS.ListenPort),
		Handler:           handler,
		ReadHeaderTimeout: time.Second * 5,
	}
	return httpServer, httpsServer
}

// serveUntilSignal 启动服务并阻塞到收到退出信号，随后优雅关闭。
func serveUntilSignal(l net.Listener, httpServer, httpsServer *http.Server) {
	// 缓冲 2 保证两个服务各写一次都不阻塞；不要 close：服务 goroutine 可能晚于 Graceful 返回才写入，
	// 关闭后再写会 panic。
	errChan := make(chan error, 2)
	err := graceful.Graceful(func() error {
		return startServers(l, httpServer, httpsServer, errChan)
	}, func(c context.Context) error {
		return shutdownServers(c, httpServer, httpsServer)
	})
	if err != nil {
		log.Printf("NEZHA>> ERROR: %v", err)
		var wrapError *utils.WrapError
		if errors.As(err, &wrapError) {
			log.Printf("NEZHA>> ERROR HTTPS: %v", wrapError.Unwrap())
		}
	}
}

// startServers 并行启动 HTTP（及可选 HTTPS）服务器，返回最先退出的那个的错误。
func startServers(l net.Listener, httpServer, httpsServer *http.Server, errChan chan error) error {
	log.Printf("NEZHA>> Dashboard::START ON %s:%d", singleton.Conf.ListenHost, singleton.Conf.ListenPort)
	if httpsServer != nil {
		go func() {
			errChan <- httpsServer.ListenAndServeTLS(singleton.Conf.HTTPS.TLSCertPath, singleton.Conf.HTTPS.TLSKeyPath)
		}()
		log.Printf("NEZHA>> Dashboard::START ON %s:%d", singleton.Conf.ListenHost, singleton.Conf.HTTPS.ListenPort)
	}
	go func() {
		errChan <- httpServer.Serve(l)
	}()
	return <-errChan
}

// shutdownServers 先落盘本小时流量并关闭 TSDB，再停止 HTTP/HTTPS；HTTPS 的错误包一层以便日志区分。
func shutdownServers(c context.Context, httpServer, httpsServer *http.Server) error {
	log.Println("NEZHA>> Graceful::START")
	singleton.RecordTransferHourlyUsage()
	singleton.CloseTSDB()
	log.Println("NEZHA>> Graceful::END")
	var err error
	if httpsServer != nil {
		err = httpsServer.Shutdown(c)
	}
	errHTTPS := errors.New("error from https server")
	return errors.Join(httpServer.Shutdown(c), utils.IfOr(err != nil, utils.NewWrapError(errHTTPS, err), nil))
}

func newHTTPandGRPCMux(httpHandler http.Handler, grpcHandler http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.ProtoMajor == 2 && r.Header.Get("Content-Type") == "application/grpc" &&
			strings.HasPrefix(r.URL.Path, "/"+proto.NezhaService_ServiceDesc.ServiceName) {
			grpcHandler.ServeHTTP(w, r)
			return
		}
		httpHandler.ServeHTTP(w, r)
	})
}
