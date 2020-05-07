package cmd

import (
	"errors"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/iqlusioninc/relayer/relayer"

	"github.com/spf13/cobra"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

/*
This command runs update-client at a preset interval.

Prometheus endpoint:
0.0.0.0:20202/metrics

Prometheus Metrics:
script_health: 1.0 if updates are running successfully, else 0.0
last_update: unix time stamp in seconds of when the last update succeded
channel_open: 1.0 if channel is open, else 0.0
unrelayed_sequences: number of unrelayed sequences or negative if error occured (check logs)

*/

func keepAliveCmd() *cobra.Command {

	var interval int
	cmd := &cobra.Command{
		Use:   "keepAlive [path]",
		Short: "Keep channel alive",
		Long:  strings.TrimSpace(`Regularily sends client updates to keep channel alive. Client ID is the same as the one used for 'raw update-client'`),
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			path, err := config.Paths.Get(args[0])
			if err != nil {
				return err
			}

			srcChainID, srcClientID := path.Src.ChainID, path.Src.ClientID
			dstChainID, dstClientID := path.Dst.ChainID, path.Dst.ClientID

			unrelayedSeq := prometheus.NewGauge(prometheus.GaugeOpts{
				Namespace: "GoZ",
				Subsystem: "relayer",
				Name:      "unrelayed_sequences",
				Help:      "number of unrelayed sequences or negative if error occurred",
			})
			prometheus.MustRegister(unrelayedSeq)

			go repeatedlyCheckUnrelayed(path, 60, unrelayedSeq)

			scriptHealthSRC := prometheus.NewGauge(prometheus.GaugeOpts{
				Namespace: "GoZ",
				Subsystem: "relayer",
				Name:      "script_health_src",
				Help:      "0.0 if script is not running successfully, else 1.0",
			})
			prometheus.MustRegister(scriptHealthSRC)

			lastUpdateSRC := prometheus.NewGauge(prometheus.GaugeOpts{
				Namespace: "GoZ",
				Subsystem: "relayer",
				Name:      "last_update_src",
				Help:      "unix timestamp in seconds of when the last update was executed",
			})
			prometheus.MustRegister(lastUpdateSRC)

			scriptHealthDST := prometheus.NewGauge(prometheus.GaugeOpts{
				Namespace: "GoZ",
				Subsystem: "relayer",
				Name:      "script_health_dst",
				Help:      "0.0 if script is not running successfully, else 1.0",
			})
			prometheus.MustRegister(scriptHealthDST)

			lastUpdateDST := prometheus.NewGauge(prometheus.GaugeOpts{
				Namespace: "GoZ",
				Subsystem: "relayer",
				Name:      "last_update_dst",
				Help:      "unix timestamp in seconds of when the last update was executed",
			})
			prometheus.MustRegister(lastUpdateDST)

			go keepAlive(interval, srcChainID, dstChainID, srcClientID, scriptHealthSRC, lastUpdateSRC)
			go keepAlive(interval, dstChainID, srcChainID, dstClientID, scriptHealthDST, lastUpdateDST)

			http.Handle("/metrics", promhttp.Handler())
			return http.ListenAndServe("0.0.0.0:20202", nil)
		},
	}

	cmd.Flags().IntVarP(&interval, "interval", "i", 5390, "interval to run update-client at")
	return cmd
}

// keepAlive runs a loop sending a client_update tx every [interval] seconds
func keepAlive(interval int, srcChainID, dstChainID, clientID string, scriptHealth, lastUpdate prometheus.Gauge) {
	for {
		err := runUpdateAtInterval(interval, srcChainID, dstChainID, clientID, scriptHealth, lastUpdate)
		log.Println("Error on update:", err)
		time.Sleep(1 * time.Second) // On error will wait a second and then try again ad infinitum
	}
}

func runUpdateAtInterval(interval int, srcChainID, dstChainID, clientID string, scriptHealth, lastUpdate prometheus.Gauge) error {
	ucc := updateClientCmd()
	args := []string{srcChainID, dstChainID, clientID}
	t := time.NewTicker(time.Second * time.Duration(interval))

	defer t.Stop()
	defer scriptHealth.Set(0.0) // set to unhealthy when funciton returns, because this only happens on error

	log.Println("Updating Client", clientID)
	if err := ucc.RunE(nil, args); err != nil {
		return err
	}

	scriptHealth.Set(1.0) // is set to healthy after succesfull execution
	lastUpdate.SetToCurrentTime()

	for range t.C {
		log.Println("Updating Clients")
		if err := ucc.RunE(nil, args); err != nil {
			return err
		}
		lastUpdate.SetToCurrentTime()
	}

	return errors.New("This shouldn't happen, this function should only return an update error")
}

func repeatedlyCheckUnrelayed(path *relayer.Path, interval int, unrelayedSeq prometheus.Gauge) {
	log.Println("Start monitoring of unrelayed sequences")
	for {
		unrelayedSeq.Set(float64(checkUnrelayedSequences(path)))
		time.Sleep(time.Duration(interval) * time.Second)
	}
}

// copy of 'query unrelayed', but returns number of unrelayed sequences or -1 on error
func checkUnrelayedSequences(path *relayer.Path) int {
	src, dst := path.Src.ChainID, path.Dst.ChainID

	c, err := config.Chains.Gets(src, dst)
	if err != nil {
		log.Println("Couldn't get unrelayed sequences:", err)
		return -1
	}

	if err = c[src].SetPath(path.Src); err != nil {
		log.Println("Couldn't get unrelayed sequences:", err)
		return -1
	}
	if err = c[dst].SetPath(path.Dst); err != nil {
		log.Println("Couldn't get unrelayed sequences:", err)
		return -1
	}

	sh, err := relayer.NewSyncHeaders(c[src], c[dst])
	if err != nil {
		log.Println("Couldn't get unrelayed sequences:", err)
		return -1
	}

	sp, err := relayer.UnrelayedSequences(c[src], c[dst], sh)
	if err != nil {
		log.Println("Couldn't get unrelayed sequences:", err)
		return -1
	}

	total := len(sp.Src) + len(sp.Dst)

	return total
}
