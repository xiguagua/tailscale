// Copyright (c) Tailscale Inc & AUTHORS
// SPDX-License-Identifier: BSD-3-Clause

//go:build !plan9

// k8s-nameserver is a simple nameserver implementation meant to be used with
// k8s-operator to allow to resolve magicDNS names associated with tailnet
// proxies in cluster.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"math/rand"
	"net"
	"net/netip"
	"os"
	"os/signal"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"syscall"

	"github.com/fsnotify/fsnotify"
	"github.com/miekg/dns"
	operatorutils "tailscale.com/k8s-operator"
	"tailscale.com/util/dnsname"
)

const (
	// hardcoded for this prototype
	tsNetDomain = "foo.tailbd97a.ts.net"
	// addr is the the address that the UDP and TCP listeners will listen on.
	addr = ":1053"

	// The following constants are specific to the nameserver configuration
	// provided by a mounted Kubernetes Configmap. The Configmap mounted at
	// /config is the only supported way for configuring this nameserver.
	defaultDNSConfigDir    = "/config"
	kubeletMountedConfigLn = "..data"
)

// nameserver is a simple nameserver that responds to DNS queries for A records
// for ts.net domain names over UDP or TCP. It serves DNS responses from
// in-memory IPv4 host records. It is intended to be deployed on Kubernetes with
// a ConfigMap mounted at /config that should contain the host records. It
// dynamically reconfigures its in-memory mappings as the contents of the
// mounted ConfigMap changes.
type nameserver struct {
	// configReader returns the latest desired configuration (host records)
	// for the nameserver. By default it gets set to a reader that reads
	// from a Kubernetes ConfigMap mounted at /config, but this can be
	// overridden in tests.
	configReader configReaderFunc
	// configWatcher is a watcher that returns an event when the desired
	// configuration has changed and the nameserver should update the
	// in-memory records.
	configWatcher <-chan string
	proxies       []string

	mu         sync.Mutex // protects following
	serviceIPs map[dnsname.FQDN][]netip.Addr
}

func main() {
	ctx, cancel := context.WithCancel(context.Background())

	// hardcoded for this prototype
	proxies := []string{"proxies-0", "proxies-1", "proxies-2", "proxies-3"}

	c := ensureWatcherForServiceConfigMaps(ctx, proxies)

	ns := &nameserver{
		configReader:  configMapConfigReader,
		configWatcher: c,
		proxies:       proxies,
	}

	ns.runServiceRecordsReconciler(ctx)

	dns.HandleFunc(tsNetDomain, ns.handleServiceNameFunc())

	// Listen for DNS queries over UDP and TCP.
	udpSig := make(chan os.Signal)
	tcpSig := make(chan os.Signal)
	go listenAndServe("udp", addr, udpSig)
	go listenAndServe("tcp", addr, tcpSig)
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	s := <-sig
	log.Printf("OS signal (%s) received, shutting down", s)
	cancel()    // exit the records reconciler and configmap watcher goroutines
	udpSig <- s // stop the UDP listener
	tcpSig <- s // stop the TCP listener
}

// handleFunc is a DNS query handler that can respond to A record queries from
// the nameserver's in-memory records.
// - If an A record query is received and the
// nameserver's in-memory records contain records for the queried domain name,
// return a success response.
// - If an A record query is received, but the
// nameserver's in-memory records do not contain records for the queried domain name,
// return NXDOMAIN.
// - If an A record query is received, but the queried domain name is not valid, return Format Error.
// - If a query is received for any other record type than A, return Not Implemented.
// func (n *nameserver) handleFunc() func(w dns.ResponseWriter, r *dns.Msg) {
// 	h := func(w dns.ResponseWriter, r *dns.Msg) {
// 		m := new(dns.Msg)
// 		defer func() {
// 			w.WriteMsg(m)
// 		}()
// 		if len(r.Question) < 1 {
// 			log.Print("[unexpected] nameserver received a request with no questions")
// 			m = r.SetRcodeFormatError(r)
// 			return
// 		}
// 		// TODO (irbekrm): maybe set message compression
// 		switch r.Question[0].Qtype {
// 		case dns.TypeA:
// 			q := r.Question[0].Name
// 			fqdn, err := dnsname.ToFQDN(q)
// 			if err != nil {
// 				m = r.SetRcodeFormatError(r)
// 				return
// 			}
// 			// The only supported use of this nameserver is as a
// 			// single source of truth for MagicDNS names by
// 			// non-tailnet Kubernetes workloads.
// 			m.Authoritative = true
// 			m.RecursionAvailable = false

//				ips := n.lookupIP4(fqdn)
//				if ips == nil || len(ips) == 0 {
//					// As we are the authoritative nameserver for MagicDNS
//					// names, if we do not have a record for this MagicDNS
//					// name, it does not exist.
//					m = m.SetRcode(r, dns.RcodeNameError)
//					return
//				}
//				// TODO (irbekrm): TTL is currently set to 0, meaning
//				// that cluster workloads will not cache the DNS
//				// records. Revisit this in future when we understand
//				// the usage patterns better- is it putting too much
//				// load on kube DNS server or is this fine?
//				for _, ip := range ips {
//					rr := &dns.A{Hdr: dns.RR_Header{Name: q, Rrtype: dns.TypeA, Class: dns.ClassINET, Ttl: 0}, A: ip}
//					m.SetRcode(r, dns.RcodeSuccess)
//					m.Answer = append(m.Answer, rr)
//				}
//			case dns.TypeAAAA:
//				// TODO (irbekrm): implement IPv6 support.
//				// Kubernetes distributions that I am most familiar with
//				// default to IPv4 for Pod CIDR ranges and often many cases don't
//				// support IPv6 at all, so this should not be crucial for now.
//				fallthrough
//			default:
//				log.Printf("[unexpected] nameserver received a query for an unsupported record type: %s", r.Question[0].String())
//				m.SetRcode(r, dns.RcodeNotImplemented)
//			}
//		}
//		return h
//	}

func (n *nameserver) handleServiceNameFunc() func(w dns.ResponseWriter, r *dns.Msg) {
	h := func(w dns.ResponseWriter, r *dns.Msg) {
		m := new(dns.Msg)
		defer func() {
			log.Print("writing message")
			if err := w.WriteMsg(m); err != nil {
				log.Printf("error writing message: %v", err)
			}
		}()
		if len(r.Question) < 1 {
			log.Print("[unexpected] nameserver received a request with no questions")
			m = r.SetRcodeFormatError(r)
			return
		}
		log.Printf("received a query for %v", r.Question[0].Name)
		switch r.Question[0].Qtype {
		case dns.TypeA:
			log.Printf("query for an A record")
			q := r.Question[0].Name
			fqdn, err := dnsname.ToFQDN(q)
			if err != nil {
				log.Print("format error")
				m = r.SetRcodeFormatError(r)
				return
			}
			m.Authoritative = true
			m.RecursionAvailable = false

			log.Print("locking service IPs")
			n.mu.Lock()
			ips := n.serviceIPs[fqdn]
			n.mu.Unlock()
			log.Print("unlocking service IPs")

			if ips == nil || len(ips) == 0 {
				log.Printf("nameserver has no IPs for %s", fqdn)
				m = m.SetRcode(r, dns.RcodeNameError)
				return
			}
			// return a random IP
			i := rand.Intn(len(ips))
			ip := ips[i]
			ipN := net.ParseIP(ip.String())
			log.Printf("produced IP address %s", ipN)
			rr := &dns.A{Hdr: dns.RR_Header{Name: q, Rrtype: dns.TypeA, Class: dns.ClassINET, Ttl: 0}, A: ipN}
			m.SetRcode(r, dns.RcodeSuccess)
			m.Answer = append(m.Answer, rr)
		case dns.TypeAAAA:
			// this prototype does not support IPv6
			fallthrough
		default:
			log.Printf("[unexpected] nameserver received a query for an unsupported record type: %s", r.Question[0].String())
			m.SetRcode(r, dns.RcodeNotImplemented)
		}
	}
	return h
}

func (n *nameserver) runServiceRecordsReconciler(ctx context.Context) {
	log.Print("updating nameserver's records from the provided services configuration...")
	if err := n.resetServiceRecords(); err != nil { // ensure records are up to date before the nameserver starts
		log.Fatalf("error setting nameserver's records: %v", err)
	}
	log.Print("nameserver's records were updated")
	go func() {
		for {
			select {
			case <-ctx.Done():
				log.Printf("context cancelled, exiting records reconciler")
				return
			case <-n.configWatcher:
				log.Print("configuration update detected, resetting records")
				if err := n.resetServiceRecords(); err != nil {
					// TODO (irbekrm): this runs in a
					// container that will be thrown away,
					// so this should be ok. But maybe still
					// need to ensure that the DNS server
					// terminates connections more
					// gracefully.
					log.Fatalf("error resetting records: %v", err)
				}
				log.Print("nameserver records were reset")
			}
		}
	}()
}

func (n *nameserver) resetServiceRecords() error {
	ip4 := make(map[dnsname.FQDN][]netip.Addr)
	for _, proxy := range n.proxies {
		dnsCfgBytes, err := proxyConfigReader(proxy)
		if err != nil {
			log.Printf("error reading proxy config for %s configuration: %v", proxy, err)
			return err
		}
		if dnsCfgBytes == nil || len(dnsCfgBytes) == 0 {
			log.Printf("configuration for proxy %s is empty; do nothing", proxy)
			continue
		}
		proxyCfg := &operatorutils.ProxyConfig{}

		err = json.Unmarshal(dnsCfgBytes, proxyCfg)
		if err != nil {
			return fmt.Errorf("error unmarshalling proxy config: %v\n", err)
		}
		for _, svc := range proxyCfg.Services {
			log.Printf("adding record for Service %s", svc.FQDN)
			ip4[dnsname.FQDN(svc.FQDN)] = append(ip4[dnsname.FQDN(svc.FQDN)], svc.V4ServiceIPs...)
		}
	}
	log.Printf("after update DNS records are %#+v", ip4)
	n.mu.Lock()
	n.serviceIPs = ip4
	n.mu.Unlock()
	return nil
}

// runRecordsReconciler ensures that nameserver's in-memory records are
// reset when the provided configuration changes.
func (n *nameserver) runRecordsReconciler(ctx context.Context) {
	log.Print("updating service records from the mounted ConfigMaps...")
	if err := n.resetServiceRecords(); err != nil {
		log.Fatalf("error setting nameserver's records: %v", err)
	}
	log.Print("service records were updated")
	go func() {
		for {
			select {
			case <-ctx.Done():
				log.Printf("context cancelled, exiting service records reconciler")
				return
			case <-n.configWatcher:
				log.Print("configuration update detected, resetting service records")
				if err := n.resetServiceRecords(); err != nil {
					log.Fatalf("error resetting records: %v", err)
				}
				log.Print("service records were reset")
			}
		}
	}()
}

// resetRecords sets the in-memory DNS records of this nameserver from the
// provided configuration. It does not check for the diff, so the caller is
// expected to ensure that this is only called when reset is needed.
// func (n *nameserver) resetRecords() error {
// 	dnsCfgBytes, err := n.configReader()
// 	if err != nil {
// 		log.Printf("error reading nameserver's configuration: %v", err)
// 		return err
// 	}
// 	if dnsCfgBytes == nil || len(dnsCfgBytes) < 1 {
// 		log.Print("nameserver's configuration is empty, any in-memory records will be unset")
// 		n.mu.Lock()
// 		n.ip4 = make(map[dnsname.FQDN][]net.IP)
// 		n.mu.Unlock()
// 		return nil
// 	}
// 	dnsCfg := &operatorutils.Records{}
// 	err = json.Unmarshal(dnsCfgBytes, dnsCfg)
// 	if err != nil {
// 		return fmt.Errorf("error unmarshalling nameserver configuration: %v\n", err)
// 	}

// 	if dnsCfg.Version != operatorutils.Alpha1Version {
// 		return fmt.Errorf("unsupported configuration version %s, supported versions are %s\n", dnsCfg.Version, operatorutils.Alpha1Version)
// 	}

// 	ip4 := make(map[dnsname.FQDN][]net.IP)
// 	defer func() {
// 		n.mu.Lock()
// 		defer n.mu.Unlock()
// 		n.ip4 = ip4
// 	}()

// 	if len(dnsCfg.IP4) == 0 {
// 		log.Print("nameserver's configuration contains no records, any in-memory records will be unset")
// 		return nil
// 	}

// 	for fqdn, ips := range dnsCfg.IP4 {
// 		fqdn, err := dnsname.ToFQDN(fqdn)
// 		if err != nil {
// 			log.Printf("invalid nameserver's configuration: %s is not a valid FQDN: %v; skipping this record", fqdn, err)
// 			continue // one invalid hostname should not break the whole nameserver
// 		}
// 		for _, ipS := range ips {
// 			ip := net.ParseIP(ipS).To4()
// 			if ip == nil { // To4 returns nil if IP is not a IPv4 address
// 				log.Printf("invalid nameserver's configuration: %v does not appear to be an IPv4 address; skipping this record", ipS)
// 				continue // one invalid IP address should not break the whole nameserver
// 			}
// 			ip4[fqdn] = []net.IP{ip}
// 		}
// 	}
// 	return nil
// }

// listenAndServe starts a DNS server for the provided network and address.
func listenAndServe(net, addr string, shutdown chan os.Signal) {
	s := &dns.Server{Addr: addr, Net: net}
	go func() {
		<-shutdown
		log.Printf("shutting down server for %s", net)
		s.Shutdown()
	}()
	log.Printf("listening for %s queries on %s", net, addr)
	if err := s.ListenAndServe(); err != nil {
		log.Fatalf("error running %s server: %v", net, err)
	}
}

// ensureWatcherForServiceConfigMaps sets up a new file watcher for the
// ConfigMaps containing records for Services served by the operator proxies.
func ensureWatcherForServiceConfigMaps(ctx context.Context, proxies []string) chan string {
	c := make(chan string)
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		log.Fatalf("error creating a new watcher for the services ConfigMap: %v", err)
	}
	go func() {
		defer watcher.Close()
		log.Printf("starting file watch for %s", "/services/")
		for {
			select {
			case <-ctx.Done():
				log.Print("context cancelled, exiting ConfigMap watcher")
				return
			case event, ok := <-watcher.Events:
				if !ok {
					log.Fatal("watcher finished; exiting")
				}
				// kubelet mounts configmap to a Pod using a series of symlinks, one of
				// which is <mount-dir>/..data that Kubernetes recommends consumers to
				// use if they need to monitor changes
				// https://github.com/kubernetes/kubernetes/blob/v1.28.1/pkg/volume/util/atomic_writer.go#L39-L61
				if strings.HasSuffix(event.Name, kubeletMountedConfigLn) {
					msg := fmt.Sprintf("ConfigMap update received: %s", event)
					log.Print(msg)
					n := path.Dir(event.Name)
					base := path.Base(n)
					c <- base // which proxy's ConfigMap should be updated
				}
			case err, ok := <-watcher.Errors:
				if err != nil {
					log.Fatalf("[unexpected] error watching services configuration: %v", err)
				}
				if !ok {
					log.Fatalf("[unexpected] errors watcher exited")
				}
			}
		}
	}()
	for _, name := range proxies {
		if err = watcher.Add(filepath.Join("/services", name)); err != nil {
			log.Fatalf("failed setting up a watcher for config for %s : %v", name, err)
		}
	}
	return c
}

// ensureWatcherForKubeConfigMap sets up a new file watcher for the ConfigMap
// that's expected to be mounted at /config. Returns a channel that receives an
// event every time the contents get updated.
func ensureWatcherForKubeConfigMap(ctx context.Context) chan string {
	c := make(chan string)
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		log.Fatalf("error creating a new watcher for the mounted ConfigMap: %v", err)
	}
	// kubelet mounts configmap to a Pod using a series of symlinks, one of
	// which is <mount-dir>/..data that Kubernetes recommends consumers to
	// use if they need to monitor changes
	// https://github.com/kubernetes/kubernetes/blob/v1.28.1/pkg/volume/util/atomic_writer.go#L39-L61
	toWatch := filepath.Join(defaultDNSConfigDir, kubeletMountedConfigLn)
	go func() {
		defer watcher.Close()
		log.Printf("starting file watch for %s", defaultDNSConfigDir)
		for {
			select {
			case <-ctx.Done():
				log.Print("context cancelled, exiting ConfigMap watcher")
				return
			case event, ok := <-watcher.Events:
				if !ok {
					log.Fatal("watcher finished; exiting")
				}
				if event.Name == toWatch {
					msg := fmt.Sprintf("ConfigMap update received: %s", event)
					log.Print(msg)
					c <- msg
				}
			case err, ok := <-watcher.Errors:
				if err != nil {
					// TODO (irbekrm): this runs in a
					// container that will be thrown away,
					// so this should be ok. But maybe still
					// need to ensure that the DNS server
					// terminates connections more
					// gracefully.
					log.Fatalf("[unexpected] error watching configuration: %v", err)
				}
				if !ok {
					// TODO (irbekrm): this runs in a
					// container that will be thrown away,
					// so this should be ok. But maybe still
					// need to ensure that the DNS server
					// terminates connections more
					// gracefully.
					log.Fatalf("[unexpected] errors watcher exited")
				}
			}
		}
	}()
	if err = watcher.Add(defaultDNSConfigDir); err != nil {
		log.Fatalf("failed setting up a watcher for the mounted ConfigMap: %v", err)
	}
	return c
}

// configReaderFunc is a function that returns the desired nameserver configuration.
type configReaderFunc func() ([]byte, error)

// configMapConfigReader reads the desired nameserver configuration from a
// records.json file in a ConfigMap mounted at /config.
var configMapConfigReader configReaderFunc = func() ([]byte, error) {
	if contents, err := os.ReadFile(filepath.Join(defaultDNSConfigDir, operatorutils.DNSRecordsCMKey)); err == nil {
		return contents, nil
	} else if os.IsNotExist(err) {
		return nil, nil
	} else {
		return nil, err
	}
}

func proxyConfigReader(proxy string) ([]byte, error) {
	path := filepath.Join("/services", proxy, "proxyConfig")
	if bs, err := os.ReadFile(path); err == nil {
		return bs, err
	} else if os.IsNotExist(err) {
		log.Printf("path %s does not exist", path)
		return nil, nil
	} else {
		return nil, fmt.Errorf("error reading %s: %w", path, err)
	}
}

// lookupIP4 returns any IPv4 addresses for the given FQDN from nameserver's
// in-memory records.
// func (n *nameserver) lookupIP4(fqdn dnsname.FQDN) []net.IP {
// 	if n.ip4 == nil {
// 		return nil
// 	}
// 	n.mu.Lock()
// 	defer n.mu.Unlock()
// 	f := n.ip4[fqdn]
// 	return f
// }
