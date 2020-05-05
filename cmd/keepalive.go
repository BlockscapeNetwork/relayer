package cmd

import (
	"errors"
	"fmt"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/cosmos/cosmos-sdk/x/ibc/04-channel/exported"
	chanTypes "github.com/cosmos/cosmos-sdk/x/ibc/04-channel/types"
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
		Use:   "keepAlive [path] [client-id]",
		Short: "Keep channel alive",
		Long:  strings.TrimSpace(`Regularily sends client updates to keep channel alive. Client ID is the same as the one used for 'raw update-client'`),
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			path, err := config.Paths.Get(args[0])
			if err != nil {
				return err
			}

			srcChainID, srcChannelID, srcPort := path.Src.ChainID, path.Src.ChannelID, path.Src.PortID
			dstChainID, dstChannelID, dstPort := path.Dst.ChainID, path.Dst.ChannelID, path.Dst.PortID

			clientID := args[1]

			unrelayedSeq := prometheus.NewGauge(prometheus.GaugeOpts{
				Namespace: "GoZ",
				Subsystem: "relayer",
				Name:      "unrelayed_sequences",
				Help:      "number of unrelayed sequences or negative if error occurred",
			})
			prometheus.MustRegister(unrelayedSeq)

			repeatedlyCheckUnrelayed(path, 60, unrelayedSeq)

			scriptHealth := prometheus.NewGauge(prometheus.GaugeOpts{
				Namespace: "GoZ",
				Subsystem: "relayer",
				Name:      "script_health",
				Help:      "0.0 if script is not running successfully, else 1.0",
			})
			prometheus.MustRegister(scriptHealth)

			lastUpdate := prometheus.NewGauge(prometheus.GaugeOpts{
				Namespace: "GoZ",
				Subsystem: "relayer",
				Name:      "last_update",
				Help:      "unix timestamp in seconds of when the last update was executed",
			})
			prometheus.MustRegister(lastUpdate)

			go keepAlive(interval, srcChainID, dstChainID, clientID, scriptHealth, lastUpdate)

			chanHealth := prometheus.NewGauge(prometheus.GaugeOpts{
				Namespace: "GoZ",
				Subsystem: "relayer",
				Name:      "channel_open",
				Help:      "1.0 if channel open in both directions, else 0.0",
			})
			prometheus.MustRegister(chanHealth)

			go repeatedlyCheckChannel(srcChainID, srcChannelID, srcPort, dstChainID, dstChannelID, dstPort, 10, chanHealth)

			http.Handle("/metrics", promhttp.Handler())
			return http.ListenAndServe("0.0.0.0:20202", nil)
		},
	}

	cmd.Flags().IntVarP(&interval, "interval", "i", 5390, "interval to run update-client at")
	return cmd
}

// keepAlive runs a loop sending a client_update tx every [interval] seconds
func keepAlive(interval int, srcChainID, dstChainID, clientID string, scriptHealth, lastUpdate prometheus.Gauge) { // TODO add cahnnel for last update time
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

	log.Println("Updating Clients")
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

func repeatedlyCheckChannel(srcChainID, srcChannelID, srcPortID, dstChainID, dstChannelID, dstPortID string, interval int, chanHealth prometheus.Gauge) {
	for {
		err := checkChannel(srcChainID, srcChannelID, srcPortID, dstChainID, dstChannelID, dstPortID)
		if err != nil {
			log.Println("Wrong channel state:", err)
			chanHealth.Set(0.0)
		} else {
			chanHealth.Set(1.0)
		}
		time.Sleep(time.Duration(interval) * time.Second)
	}
}

func checkChannel(srcChainID, srcChannelID, srcPortID, dstChainID, dstChannelID, dstPortID string) error {
	if err := checkChanState(srcChainID, srcChannelID, srcPortID); err != nil {
		return err
	}

	if err := checkChanState(dstChainID, dstChannelID, dstPortID); err != nil {
		return err
	}

	return nil
}

// returns nil if channel is open, else error
func checkChanState(chainID, channelID, portID string) error {
	res, err := queryChan(chainID, channelID, portID)
	if err != nil {
		return err
	}
	s := res.Channel.Channel.GetState()
	if s != exported.OPEN {
		return fmt.Errorf("Expected src channel state to be %s but was %s", exported.OPEN, s)
	}
	return nil
}

// copy of queryChannel which returns the response instead of printing it
func queryChan(chainID, channelID, portID string) (chanTypes.ChannelResponse, error) {
	emptyRes := chanTypes.ChannelResponse{}

	chain, err := config.Chains.Get(chainID)
	if err != nil {
		return emptyRes, err
	}

	if err = chain.AddPath(dcli, dcon, channelID, portID, dord); err != nil {
		return emptyRes, err
	}

	height, err := chain.QueryLatestHeight()
	if err != nil {
		return emptyRes, err
	}

	return chain.QueryChannel(height)
}

func repeatedlyCheckUnrelayed(path *relayer.Path, interval int, unrelayedSeq prometheus.Gauge) {
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
