package cmd

import (
	"bufio"
	"fmt"
	"net/http"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/cosmos/cosmos-sdk/client/flags"
	"github.com/cosmos/cosmos-sdk/crypto/keyring"
	"github.com/cosmos/cosmos-sdk/std"
	authtypes "github.com/cosmos/cosmos-sdk/x/auth/types"
	"github.com/go-kit/kit/transport/http/jsonrpc"
	"github.com/gorilla/mux"
	"github.com/rs/cors"
	"github.com/sentinel-official/hub"
	"github.com/sentinel-official/hub/params"
	"github.com/spf13/cobra"
	"github.com/spf13/viper"
	rpchttp "github.com/tendermint/tendermint/rpc/client/http"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"

	"github.com/sentinel-official/dvpn-node/context"
	"github.com/sentinel-official/dvpn-node/lite"
	"github.com/sentinel-official/dvpn-node/node"
	"github.com/sentinel-official/dvpn-node/rest"
	"github.com/sentinel-official/dvpn-node/services/wireguard"
	wgtypes "github.com/sentinel-official/dvpn-node/services/wireguard/types"
	"github.com/sentinel-official/dvpn-node/types"
	"github.com/sentinel-official/dvpn-node/utils"
)

func runHandshake(peers uint64) error {
	return exec.Command("hnsd",
		strings.Split(fmt.Sprintf("--log-file /dev/null "+
			"--pool-size %d "+
			"--rs-host 0.0.0.0:53", peers), " ")...).Run()
}

func StartCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "start",
		Short: "Start the VPN node",
		RunE: func(cmd *cobra.Command, _ []string) error {
			var (
				home         = viper.GetString(flags.FlagHome)
				configPath   = filepath.Join(home, types.ConfigFileName)
				databasePath = filepath.Join(home, types.DatabaseFileName)
			)

			log, err := utils.PrepareLogger()
			if err != nil {
				return err
			}

			v := viper.New()
			v.SetConfigFile(configPath)

			log.Info("Reading the configuration file", "path", configPath)
			config, err := types.ReadInConfig(v)
			if err != nil {
				return err
			}

			validateConfig, err := cmd.Flags().GetBool(flagEnableConfigValidation)
			if err != nil {
				return err
			}

			if validateConfig {
				log.Info("Validating the configuration", "data", config)
				if err := config.Validate(); err != nil {
					return err
				}
			}

			log.Info("Creating IPv4 pool", "CIDR", types.IPv4CIDR)
			ipv4Pool, err := wgtypes.NewIPv4PoolFromCIDR(types.IPv4CIDR)
			if err != nil {
				return err
			}

			log.Info("Creating IPv6 pool", "CIDR", types.IPv6CIDR)
			ipv6Pool, err := wgtypes.NewIPv6PoolFromCIDR(types.IPv6CIDR)
			if err != nil {
				return err
			}

			var (
				encoding = params.MakeEncodingConfig()
				service  = wireguard.NewWireGuard(wgtypes.NewIPPool(ipv4Pool, ipv6Pool))
				reader   = bufio.NewReader(cmd.InOrStdin())
			)

			std.RegisterInterfaces(encoding.InterfaceRegistry)
			hub.ModuleBasics.RegisterInterfaces(encoding.InterfaceRegistry)

			log.Info("Initializing RPC HTTP client", "address", config.Chain.RPCAddress, "endpoint", "/websocket")
			rpcclient, err := rpchttp.New(config.Chain.RPCAddress, "/websocket")
			if err != nil {
				return err
			}

			log.Info("Initializing keyring", "name", types.KeyringName, "backend", config.Keyring.Backend)
			kr, err := keyring.New(types.KeyringName, config.Keyring.Backend, home, reader)
			if err != nil {
				return err
			}

			info, err := kr.Key(config.Keyring.From)
			if err != nil {
				return err
			}

			client := lite.NewDefaultClient().
				WithAccountRetriever(authtypes.AccountRetriever{}).
				WithChainID(config.Chain.ID).
				WithClient(rpcclient).
				WithFrom(config.Keyring.From).
				WithFromAddress(info.GetAddress()).
				WithFromName(config.Keyring.From).
				WithGas(config.Chain.Gas).
				WithGasAdjustment(config.Chain.GasAdjustment).
				WithGasPrices(config.Chain.GasPrices).
				WithInterfaceRegistry(encoding.InterfaceRegistry).
				WithKeyring(kr).
				WithLegacyAmino(encoding.Amino).
				WithLogger(log).
				WithNodeURI(config.Chain.RPCAddress).
				WithSimulateAndExecute(config.Chain.SimulateAndExecute).
				WithTxConfig(encoding.TxConfig)

			for {
			account, err := client.QueryAccount(info.GetAddress())
			if err != nil {
			    return err
			}
			if account == nil {
			    log.Error("account does not exist with address %s", client.FromAddress())
			    time.Sleep(30*time.Second)
			    continue
			}
			break
			}

			log.Info("Fetching GeoIP location info...")
			location, err := utils.FetchGeoIPLocation()
			if err != nil {
				return err
			}
			log.Info("GeoIP location info", "city", location.City, "country", location.Country)

			log.Info("Performing internet speed test...")
			bandwidth, err := utils.Bandwidth()
			if err != nil {
				return err
			}
			log.Info("Internet speed test result", "data", bandwidth)

			if config.Handshake.Enable {
				go func() {
					for {
						log.Info("Starting the Handshake process...")
						if err := runHandshake(config.Handshake.Peers); err != nil {
							log.Error("Handshake process exited unexpectedly", "error", err)
						}
					}
				}()
			}

			log.Info("Initializing VPN service", "type", service.Type())
			if err := service.Init(home); err != nil {
				return err
			}

			log.Info("Starting VPN service", "type", service.Type())
			if err := service.Start(); err != nil {
				return err
			}

			log.Info("Opening the database", "path", databasePath)
			database, err := gorm.Open(
				sqlite.Open(databasePath),
				&gorm.Config{
					Logger:      logger.Discard,
					PrepareStmt: false,
				},
			)
			if err != nil {
				return err
			}

			log.Info("Migrating database models...")
			if err := database.AutoMigrate(&types.Session{}); err != nil {
				return err
			}

			var (
				corsRouter = cors.New(
					cors.Options{
						AllowedMethods: []string{
							http.MethodGet,
							http.MethodPost,
						},
						AllowedHeaders: []string{
							jsonrpc.ContentType,
						},
					},
				)
				ctx       = context.NewContext()
				muxRouter = mux.NewRouter()
			)

			rest.RegisterRoutes(ctx, muxRouter)

			ctx = ctx.
				WithLogger(log).
				WithService(service).
				WithHandler(corsRouter.Handler(muxRouter)).
				WithConfig(config).
				WithClient(client).
				WithLocation(location).
				WithDatabase(database).
				WithBandwidth(bandwidth)

			n := node.NewNode(ctx)
			if err := n.Initialize(); err != nil {
				return err
			}

			return n.Start()
		},
	}

	cmd.Flags().Bool(flagEnableConfigValidation, true, "enable the validation of configuration")

	return cmd
}
