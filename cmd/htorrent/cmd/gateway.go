package cmd

import (
	"context"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"

	v1 "github.com/pojntfx/htorrent/pkg/api/http/v1"
	"github.com/pojntfx/htorrent/pkg/server"
	"github.com/rs/zerolog/log"
	"github.com/spf13/cobra"
	"github.com/spf13/viper"
)

const (
	storageFlag      = "storage"
	laddrFlag        = "laddr"
	apiUsernameFlag  = "api-username"
	apiPasswordFlag  = "api-password"
	oidcIssuerFlag   = "oidc-issuer"
	oidcClientIDFlag = "oidc-client-id"
	// New flags
	maxPeersFlag     = "max-peers"
	dhtFlag          = "dht"
	upnpFlag         = "upnp"
	protocolsFlag    = "protocols"
	downloadDirFlag  = "download-dir"
)

var gatewayCmd = &cobra.Command{
	Use:     "gateway",
	Aliases: []string{"g"},
	Short:   "Start a gateway",
	RunE: func(cmd *cobra.Command, args []string) error {
		if err := viper.BindPFlags(cmd.PersistentFlags()); err != nil {
			return err
		}

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		addr, err := net.ResolveTCPAddr("tcp", viper.GetString(laddrFlag))
		if err != nil {
			return err
		}

		if port := os.Getenv("PORT"); port != "" {
			log.Debug().Msg("Using port from PORT env variable")

			p, err := strconv.Atoi(port)
			if err != nil {
				return err
			}

			addr.Port = p
		}

		// Parse protocols from comma-separated string to slice
		protocolsStr := viper.GetString(protocolsFlag)
		var protocols []string
		if protocolsStr != "" {
			protocols = strings.Split(protocolsStr, ",")
			for i, p := range protocols {
				protocols[i] = strings.TrimSpace(p)
			}
		}

		gateway := server.NewGateway(
			addr.String(),
					     viper.GetString(storageFlag),
					     viper.GetInt(verboseFlag) > 5,
					     // New parameters
					     viper.GetInt(maxPeersFlag),
					     viper.GetBool(dhtFlag),
					     viper.GetBool(upnpFlag),
					     protocols,
			       viper.GetString(downloadDirFlag),
					     func(torrentMetrics v1.TorrentMetrics, fileMetrics v1.FileMetrics) {
						     log.Debug().
						     Str("magnet", torrentMetrics.Magnet).
						     Int("peers", torrentMetrics.Peers).
						     Str("path", fileMetrics.Path).
						     Int64("length", fileMetrics.Length).
						     Int64("completed", fileMetrics.Completed).
						     Msg("Streaming")
					     },
			       ctx,
		)

		if err := gateway.Open(); err != nil {
			return err
		}

		s := make(chan os.Signal)
		signal.Notify(s, os.Interrupt, syscall.SIGTERM)
		go func() {
			<-s

			log.Debug().Msg("Gracefully shutting down")

			go func() {
				<-s

				log.Debug().Msg("Forcing shutdown")

				cancel()

				os.Exit(1)
			}()

			if err := gateway.Close(); err != nil {
				panic(err)
			}

			cancel()
		}()

		log.Info().
		Str("address", addr.String()).
		Msg("Listening")

		return gateway.Wait()
	},
}

func init() {
	home, err := os.UserHomeDir()
	if err != nil {
		panic(err)
	}

	gatewayCmd.PersistentFlags().StringP(storageFlag, "s", filepath.Join(home, ".local", "share", "htorrent", "var", "lib", "htorrent", "data"), "Path to store downloaded torrents in")
	gatewayCmd.PersistentFlags().StringP(laddrFlag, "l", ":1337", "Listening address")
	gatewayCmd.PersistentFlags().String(apiUsernameFlag, "admin", "Username for the management API (can also be set using the API_USERNAME env variable). Ignored if any of the OIDC parameters are set.")
	gatewayCmd.PersistentFlags().String(apiPasswordFlag, "", "Password for the management API (can also be set using the API_PASSWORD env variable). Ignored if any of the OIDC parameters are set.")
	gatewayCmd.PersistentFlags().String(oidcIssuerFlag, "", "OIDC Issuer (i.e. https://pojntfx.eu.auth0.com/) (can also be set using the OIDC_ISSUER env variable)")
	gatewayCmd.PersistentFlags().String(oidcClientIDFlag, "", "OIDC Client ID (i.e. myoidcclientid) (can also be set using the OIDC_CLIENT_ID env variable)")

	// New flags
	gatewayCmd.PersistentFlags().IntP(maxPeersFlag, "m", 50, "Maximum number of peers to connect to")
	gatewayCmd.PersistentFlags().BoolP(dhtFlag, "d", true, "Enable DHT (Distributed Hash Table)")
	gatewayCmd.PersistentFlags().BoolP(upnpFlag, "u", true, "Enable UPnP port forwarding on router")
	gatewayCmd.PersistentFlags().StringP(protocolsFlag, "p", "tcp,utp", "Comma-separated list of protocols to use (tcp, utp)")
	gatewayCmd.PersistentFlags().StringP(downloadDirFlag, "o", "", "Directory to download torrents to (defaults to storage directory if not specified)")

	viper.AutomaticEnv()

	rootCmd.AddCommand(gatewayCmd)
}
