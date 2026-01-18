package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path"
	"time"

	"github.com/anacrolix/torrent"
	"github.com/anacrolix/torrent/storage"
	"github.com/phayes/freeport"
	v1 "github.com/pojntfx/htorrent/pkg/api/http/v1"
	"github.com/rs/zerolog/log"
)

var (
	ErrEmptyMagnetLink  = errors.New("could not work with empty magnet link")
	ErrEmptyPath        = errors.New("could not work with empty path")
	ErrCouldNotFindPath = errors.New("could not find path in torrent")
)

type Gateway struct {
	laddr       string
	storage     string
	debug       bool

	// New fields
	maxPeers     int
	dht          bool
	upnp         bool
	protocols    []string
	downloadDir  string

	onDownloadProgress func(torrentMetrics v1.TorrentMetrics, fileMetrics v1.FileMetrics)

	torrentClient *torrent.Client
	srv           *http.Server

	errs chan error

	ctx context.Context
}

func NewGateway(
	laddr string,
	storage string,
	debug bool,
	// New parameters
	maxPeers int,
	dht bool,
	upnp bool,
	protocols []string,
	downloadDir string,
	onDownloadProgress func(torrentMetrics v1.TorrentMetrics, fileMetrics v1.FileMetrics),
		ctx context.Context,
) *Gateway {
	return &Gateway{
		laddr:   laddr,
		storage: storage,
		debug:   debug,

		// New fields initialization
		maxPeers:    maxPeers,
		dht:         dht,
		upnp:        upnp,
		protocols:   protocols,
		downloadDir: downloadDir,

		onDownloadProgress: onDownloadProgress,

		errs: make(chan error),

		ctx: ctx,
	}
}

func (g *Gateway) Open() error {
	log.Trace().Msg("Opening gateway")

	cfg := torrent.NewDefaultClientConfig()
	cfg.Debug = g.debug

	// Determine the download directory
	// If downloadDir is not specified, use the storage directory
	downloadBaseDir := g.downloadDir
	if downloadBaseDir == "" {
		downloadBaseDir = g.storage
		log.Info().Str("downloadDir", downloadBaseDir).Msg("Using storage directory for downloads")
	} else {
		// Ensure download directory exists
		if err := os.MkdirAll(downloadBaseDir, 0755); err != nil {
			log.Error().Err(err).Str("downloadDir", downloadBaseDir).Msg("Failed to create download directory")
			return err
		}
		log.Info().Str("downloadDir", downloadBaseDir).Msg("Using specified download directory")
	}

	// Configure storage to use the download directory
	cfg.DefaultStorage = storage.NewFile(downloadBaseDir)

	torrentPort, err := freeport.GetFreePort()
	if err != nil {
		panic(err)
	}
	cfg.ListenPort = torrentPort

	// Configure maximum peers
	if g.maxPeers > 0 {
		cfg.EstablishedConnsPerTorrent = g.maxPeers
		log.Info().Int("maxPeers", g.maxPeers).Msg("Maximum peers configured")
	}

	// Configure DHT
	cfg.NoDHT = !g.dht
	if g.dht {
		log.Info().Msg("DHT enabled")
	} else {
		log.Info().Msg("DHT disabled")
	}

	// Configure UPnP
	cfg.NoDefaultPortForwarding = !g.upnp
	if g.upnp {
		log.Info().Msg("UPnP port forwarding enabled")
	} else {
		log.Info().Msg("UPnP port forwarding disabled")
	}

	// Configure protocols
	if len(g.protocols) > 0 {
		// Start with all protocols disabled
		cfg.DisableTCP = true
		cfg.DisableUTP = true

		for _, protocol := range g.protocols {
			switch protocol {
				case "tcp":
					cfg.DisableTCP = false
					log.Info().Msg("TCP protocol enabled")
				case "utp":
					cfg.DisableUTP = false
					log.Info().Msg("uTP protocol enabled")
				default:
					log.Warn().Str("protocol", protocol).Msg("Unknown protocol, skipping")
			}
		}

		// If no valid protocols were specified, enable both as fallback
		if cfg.DisableTCP && cfg.DisableUTP {
			cfg.DisableTCP = false
			cfg.DisableUTP = false
			log.Warn().Msg("No valid protocols specified, enabling both TCP and uTP")
		}
	}

	// Set peer connection parameters
	cfg.MinPeerExtensions.SetBit(0, true)

	c, err := torrent.NewClient(cfg)
	if err != nil {
		return err
	}
	g.torrentClient = c

	mux := http.NewServeMux()

	mux.HandleFunc("/info", func(w http.ResponseWriter, r *http.Request) {
		magnetLink := r.URL.Query().Get("magnet")
		if magnetLink == "" {
			w.WriteHeader(http.StatusUnprocessableEntity)

			panic(ErrEmptyMagnetLink)
		}

		log.Debug().
		Str("magnet", magnetLink).
		Msg("Getting info")

		t, err := c.AddMagnet(magnetLink)
		if err != nil {
			panic(err)
		}
		<-t.GotInfo()

		info := v1.Info{
			Files: []v1.File{},
		}
		info.Name = t.Info().BestName()
		info.InfoHash = t.InfoHash().HexString()
		info.CreationDate = t.Metainfo().CreationDate

		foundDescription := false
		for _, f := range t.Files() {
			log.Debug().
			Str("magnet", magnetLink).
			Str("path", f.Path()).
			Msg("Got info")

			info.Files = append(info.Files, v1.File{
				Path:   f.Path(),
					    Length: f.Length(),
			})

			if path.Ext(f.Path()) == ".txt" {
				if foundDescription {
					continue
				}

				r := f.NewReader()
				defer r.Close()

				var description bytes.Buffer
				if _, err := io.Copy(&description, r); err != nil {
					panic(err)
				}

				info.Description = description.String()

				foundDescription = true
			}
		}

		enc := json.NewEncoder(w)
		if err := enc.Encode(info); err != nil {
			panic(err)
		}
	})

	mux.HandleFunc("/metrics", func(w http.ResponseWriter, r *http.Request) {
		log.Debug().
		Msg("Getting metrics")

		metrics := []v1.TorrentMetrics{}
		for _, t := range g.torrentClient.Torrents() {
			mi := t.Metainfo()

			info, err := mi.UnmarshalInfo()
			if err != nil {
				log.Error().
				Err(err).
				Msg("Could not unmarshal metainfo")

				continue
			}

			fileMetrics := []v1.FileMetrics{}
			for _, f := range t.Files() {
				fileMetrics = append(fileMetrics, v1.FileMetrics{
					Path:      f.Path(),
						     Length:    f.Length(),
						     Completed: f.BytesCompleted(),
				})
			}

			torrentMetrics := v1.TorrentMetrics{
				Magnet:   mi.Magnet(nil, &info).String(),
		       InfoHash: mi.HashInfoBytes().HexString(),
		       Peers:    len(t.PeerConns()),
		       Files:    fileMetrics,
			}

			metrics = append(metrics, torrentMetrics)
		}

		enc := json.NewEncoder(w)
		if err := enc.Encode(metrics); err != nil {
			panic(err)
		}
	})

	mux.HandleFunc("/stream", func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			err := recover()

			switch err {
				default:
					w.WriteHeader(http.StatusInternalServerError)

					e, ok := err.(error)
					if ok {
						log.Debug().
						Err(e).
						Msg("Closed connection for client")
					} else {
						log.Debug().Msg("Closed connection for client")
					}
			}
		}()

		magnetLink := r.URL.Query().Get("magnet")
		if magnetLink == "" {
			w.WriteHeader(http.StatusUnprocessableEntity)

			panic(ErrEmptyMagnetLink)
		}

		requestedPath := r.URL.Query().Get("path")
		if requestedPath == "" {
			w.WriteHeader(http.StatusUnprocessableEntity)

			panic(ErrEmptyPath)
		}

		log.Debug().
		Str("magnet", magnetLink).
		Str("path", requestedPath).
		Msg("Getting stream")

		t, err := c.AddMagnet(magnetLink)
		if err != nil {
			panic(err)
		}
		<-t.GotInfo()

		found := false
		for _, l := range t.Files() {
			f := l

			if f.Path() != requestedPath {
				continue
			}

			found = true

			go func() {
				tick := time.NewTicker(time.Millisecond * 100)
				defer tick.Stop()

				lastCompleted := int64(0)
				for range tick.C {
					if completed, length := f.BytesCompleted(), f.Length(); completed < length {
						if completed != lastCompleted {
							g.onDownloadProgress(
								v1.TorrentMetrics{
									Magnet: magnetLink,
									Peers:  len(f.Torrent().PeerConns()),
									     Files:  []v1.FileMetrics{},
								},
			    v1.FileMetrics{
				    Path:      f.Path(),
									     Length:    length,
									     Completed: completed,
			    },
							)
						}

						lastCompleted = completed
					} else {
						return
					}
				}
			}()

			log.Debug().
			Str("magnet", magnetLink).
			Str("path", requestedPath).
			Msg("Got stream")

			http.ServeContent(w, r, f.DisplayPath(), time.Unix(f.Torrent().Metainfo().CreationDate, 0), f.NewReader())
		}

		if !found {
			w.WriteHeader(http.StatusNotFound)

			panic(ErrCouldNotFindPath)
		}
	})

	g.srv = &http.Server{Addr: g.laddr}
	g.srv.Handler = mux

	log.Debug().
	Str("address", g.laddr).
	Msg("Listening")

	go func() {
		if err := g.srv.ListenAndServe(); err != nil {
			if err == http.ErrServerClosed {
				close(g.errs)

				return
			}

			g.errs <- err

			return
		}
	}()

	return nil
}

func (g *Gateway) Close() error {
	log.Trace().Msg("Closing gateway")

	if err := g.srv.Shutdown(g.ctx); err != nil {
		if err != context.Canceled {
			return err
		}
	}

	errs := g.torrentClient.Close()
	for _, err := range errs {
		if err != nil {
			if err != context.Canceled {
				return err
			}
		}
	}

	return nil
}

func (g *Gateway) Wait() error {
	for err := range g.errs {
		if err != nil {
			return err
		}
	}

	return nil
}
