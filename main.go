//go:generate bash -c "mkdir -p codegen && go run github.com/deepmap/oapi-codegen/cmd/oapi-codegen@v1.12.4 -generate types,server,spec -package codegen api/casaos/openapi.yaml > codegen/casaos_api.go"

package main

import (
	_ "embed"
	"flag"
	"fmt"
	"net"
	"net/http"
	"path/filepath"
	"time"

	"github.com/IceWhaleTech/CasaOS-Common/model"
	"github.com/IceWhaleTech/CasaOS-Common/utils/constants"
	"github.com/IceWhaleTech/CasaOS-Common/utils/logger"

	util_http "github.com/IceWhaleTech/CasaOS-Common/utils/http"

	"github.com/IceWhaleTech/CasaOS/pkg/cache"
	"github.com/IceWhaleTech/CasaOS/pkg/config"
	"github.com/IceWhaleTech/CasaOS/pkg/sqlite"
	"github.com/IceWhaleTech/CasaOS/pkg/utils/command"
	"github.com/IceWhaleTech/CasaOS/pkg/utils/file"
	"github.com/IceWhaleTech/CasaOS/route"
	"github.com/IceWhaleTech/CasaOS/service"
	"github.com/IceWhaleTech/CasaOS/types"
	"github.com/coreos/go-systemd/daemon"
	"go.uber.org/zap"

	"github.com/robfig/cron"
	"gorm.io/gorm"
)

const LOCALHOST = "127.0.0.1"

var sqliteDB *gorm.DB

var (
	commit = "private build"
	date   = "private build"

	//go:embed api/index.html
	_docHTML string

	//go:embed api/casaos/openapi.yaml
	_docYAML string

	configFlag  = flag.String("c", "", "config address")
	dbFlag      = flag.String("db", "", "db path")
	versionFlag = flag.Bool("v", false, "version")
)

func init() {
	flag.Parse()
	if *versionFlag {
		fmt.Println("v" + types.CURRENTVERSION)
		return
	}

	println("git commit:", commit)
	println("build date:", date)

	config.InitSetup(*configFlag)

	logger.LogInit(config.AppInfo.LogPath, config.AppInfo.LogSaveName, config.AppInfo.LogFileExt)
	if len(*dbFlag) == 0 {
		*dbFlag = config.AppInfo.DBPath + "/db"
	}

	sqliteDB = sqlite.GetDb(*dbFlag)
	// gredis.GetRedisConn(config.RedisInfo),

	service.MyService = service.NewService(sqliteDB, config.CommonInfo.RuntimePath, route.SocketIo())

	service.Cache = cache.Init()

	service.GetCPUThermalZone()

	route.InitFunction()
}

// @title casaOS API
// @version 1.0.0
// @contact.name lauren.pan
// @contact.url https://www.zimaboard.com
// @contact.email lauren.pan@icewhale.org
// @description casaOS v1版本api
// @host 192.168.2.217:8089
// @securityDefinitions.apikey ApiKeyAuth
// @in header
// @name Authorization
// @BasePath /v1
func main() {
	if *versionFlag {
		return
	}

	v1Router := route.InitV1Router()
	defer service.SocketServer.Close()

	v2Router := route.InitV2Router()
	v2DocRouter := route.InitV2DocRouter(_docHTML, _docYAML)

	mux := &util_http.HandlerMultiplexer{
		HandlerMap: map[string]http.Handler{
			"v1":  v1Router,
			"v2":  v2Router,
			"doc": v2DocRouter,
		},
	}

	cron2 := cron.New()
	// every day execution

	err := cron2.AddFunc("0/5 * * * * *", func() {
		if service.ClientCount > 0 {
			// route.SendNetINfoBySocket()
			// route.SendCPUBySocket()
			// route.SendMemBySocket()
			// route.SendDiskBySocket()
			// route.SendUSBBySocket()
			route.SendAllHardwareStatusBySocket()
		}
	})
	if err != nil {
		fmt.Println(err)
	}
	cron2.Start()

	defer cron2.Stop()

	listener, err := net.Listen("tcp", net.JoinHostPort(LOCALHOST, "0"))
	if err != nil {
		panic(err)
	}
	apiPaths := []string{
		"/v1/sys",
		"/v1/port",
		"/v1/file",
		"/v1/folder",
		"/v1/batch",
		"/v1/image",
		"/v1/samba",
		"/v1/notify",
		"/v1/socketio",
		route.V2APIPath,
		route.V2DocPath,
	}
	for _, apiPath := range apiPaths {
		err = service.MyService.Gateway().CreateRoute(&model.Route{
			Path:   apiPath,
			Target: "http://" + listener.Addr().String(),
		})

		if err != nil {
			fmt.Println("err", err)
			panic(err)
		}
	}
	go func() {
		time.Sleep(time.Second * 2)
		// v0.3.6
		if config.ServerInfo.HttpPort != "" {
			changePort := model.ChangePortRequest{}
			changePort.Port = config.ServerInfo.HttpPort
			err := service.MyService.Gateway().ChangePort(&changePort)
			if err == nil {
				config.Cfg.Section("server").Key("HttpPort").SetValue("")
				config.Cfg.SaveTo(config.SystemConfigInfo.ConfigPath)
			}
		}
	}()

	urlFilePath := filepath.Join(config.CommonInfo.RuntimePath, "casaos.url")
	if err := file.CreateFileAndWriteContent(urlFilePath, "http://"+listener.Addr().String()); err != nil {
		logger.Error("error when creating address file", zap.Error(err),
			zap.Any("address", listener.Addr().String()),
			zap.Any("filepath", urlFilePath),
		)
	}

	// run any script that needs to be executed
	scriptDirectory := filepath.Join(constants.DefaultConfigPath, "start.d")
	command.ExecuteScripts(scriptDirectory)

	if supported, err := daemon.SdNotify(false, daemon.SdNotifyReady); err != nil {
		logger.Error("Failed to notify systemd that casaos main service is ready", zap.Any("error", err))
	} else if supported {
		logger.Info("Notified systemd that casaos main service is ready")
	} else {
		logger.Info("This process is not running as a systemd service.")
	}

	s := &http.Server{
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second, // fix G112: Potential slowloris attack (see https://github.com/securego/gosec)
	}

	logger.Info("CasaOS main service is listening...", zap.Any("address", listener.Addr().String()))

	err = s.Serve(listener) // not using http.serve() to fix G114: Use of net/http serve function that has no support for setting timeouts (see https://github.com/securego/gosec)
	if err != nil {
		panic(err)
	}
}
