package maprobe

import (
	"context"
	"encoding/json"
	"log"
	"reflect"
	"sync"
	"time"

	mackerel "github.com/mackerelio/mackerel-client-go"
)

var (
	MaxConcurrency         = 100
	PostMetricBufferLength = 100
	sem                    = make(chan struct{}, MaxConcurrency)
	ProbeInterval          = 60 * time.Second
	mackerelRetryInterval  = 10 * time.Second
)

func lock() {
	sem <- struct{}{}
	log.Printf("[trace] locked. concurrency: %d", len(sem))
}

func unlock() {
	<-sem
	log.Printf("[trace] unlocked. concurrency: %d", len(sem))
}

func Run(ctx context.Context, wg *sync.WaitGroup, configPath string) error {
	defer wg.Done()
	log.Println("[info] starting maprobe")
	conf, err := LoadConfig(configPath)
	if err != nil {
		return err
	}
	log.Println("[debug]", conf.String())
	client := mackerel.NewClient(conf.APIKey)
	ch := make(chan Metric, PostMetricBufferLength*10)

	if conf.ProbeOnly {
		go dumpMetricWorker(ctx, ch)
	} else {
		go postMetricWorker(ctx, client, ch)
	}

	ticker := time.NewTicker(ProbeInterval)
	for {
		var wg2 sync.WaitGroup
		for _, pd := range conf.Probes {
			wg2.Add(1)
			go runProbes(ctx, pd, client, ch, &wg2)
		}
		wg2.Wait()

		log.Println("[debug] waiting for a next tick")
		select {
		case <-ctx.Done():
			log.Println("[info] stopping maprobe")
			return nil
		case <-ticker.C:
		}

		log.Println("[debug] checking a new config")
		newConf, err := LoadConfig(configPath)
		if err != nil {
			log.Println("[warn]", err)
			log.Println("[warn] still using current config")
		} else if !reflect.DeepEqual(conf, newConf) {
			conf = newConf
			log.Println("[info] config reloaded")
			log.Println("[debug]", conf)
		}
	}
	return nil
}

func runProbes(ctx context.Context, pd *ProbeDefinition, client *mackerel.Client, ch chan Metric, wg *sync.WaitGroup) {
	defer wg.Done()
	log.Printf("[debug] finding hosts service:%s roles:%s", pd.Service, pd.Roles)
	hosts, err := client.FindHosts(&mackerel.FindHostsParam{
		Service: pd.Service,
		Roles:   pd.Roles,
	})
	if err != nil {
		log.Println("[error]", err)
		return
	}
	log.Printf("[debug] %d hosts found", len(hosts))
	if len(hosts) == 0 {
		return
	}

	spawnInterval := time.Duration(int64(ProbeInterval) / int64(len(hosts)) / 2)
	if spawnInterval > time.Second {
		spawnInterval = time.Second
	}

	var wg2 sync.WaitGroup
	for _, host := range hosts {
		time.Sleep(spawnInterval)
		log.Printf("[debug] preparing host id:%s name:%s", host.ID, host.Name)
		wg2.Add(1)
		go func(host *mackerel.Host) {
			lock()
			defer unlock()
			defer wg2.Done()
			for _, probe := range pd.GenerateProbes(host) {
				log.Printf("[debug] probing host id:%s name:%s probe:%s", host.ID, host.Name, probe)
				metrics, err := probe.Run(ctx)
				if err != nil {
					log.Printf("[warn] probe failed. %s host id:%s name:%s probe:%s", err, host.ID, host.Name, probe)
				}
				for _, m := range metrics {
					ch <- m
				}
			}
		}(host)
	}
	wg2.Wait()
}

func postMetricWorker(ctx context.Context, client *mackerel.Client, ch chan Metric) {
	ticker := time.NewTicker(10 * time.Second)
	mvs := make([]*mackerel.HostMetricValue, 0, PostMetricBufferLength)
	for {
		select {
		case <-ctx.Done():
		case <-ticker.C:
		case m := <-ch:
			mvs = append(mvs, m.HostMetricValue())
			if len(mvs) < PostMetricBufferLength {
				continue
			}
		}
		if len(mvs) == 0 {
			continue
		}
		log.Printf("[debug] posting %d metrics to Mackerel", len(mvs))
		b, _ := json.Marshal(mvs)
		log.Println("[debug]", string(b))
		if err := client.PostHostMetricValues(mvs); err != nil {
			log.Println("[error] failed to post metrics to Mackerel", err)
			time.Sleep(mackerelRetryInterval)
			continue
		}
		log.Printf("[debug] post succeeded.")
		// success. reset buffer
		mvs = mvs[:0]
	}
}

func dumpMetricWorker(ctx context.Context, ch chan Metric) {
	for {
		select {
		case <-ctx.Done():
		case m := <-ch:
			b, _ := json.Marshal(m.HostMetricValue())
			log.Println("[debug]", string(b))
		}
	}
}

type templateParam struct {
	Host *mackerel.Host
}
