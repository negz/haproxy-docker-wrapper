// Copyright © 2018 Tuenti Technologies S.L.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package main

import (
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"
)

var version = "dev"
var configTimeout = 5 * time.Minute

func watchHaproxyStart(haproxy HaproxyServer) chan bool {
	started := make(chan bool)
	go func() {
		for {
			if haproxy.IsRunning() {
				started <- true
				return
			}
			<-time.After(1 * time.Second)
		}
	}()
	return started
}

func main() {
	var haproxyPath, haproxyPIDFile, haproxyConfigFile, controlAddress, haproxyMode string
	var syslogPort uint
	var showVersion bool
	flag.UintVar(&syslogPort, "syslog-port", 514, "Port for embedded syslog server")
	flag.StringVar(&haproxyPath, "haproxy", "/usr/local/sbin/haproxy", "Path to haproxy binary")
	flag.StringVar(&haproxyPIDFile, "haproxy-pidfile", "/var/run/haproxy.pid", "Pidfile for haproxy")
	flag.StringVar(&controlAddress, "control-address", "127.0.0.1:15000", "HTTP port for controller commands")
	flag.StringVar(&haproxyConfigFile, "haproxy-config", "/usr/local/etc/haproxy/haproxy.cfg", "Path to configuration file for haproxy")
	flag.StringVar(&haproxyMode, "haproxy-mode", "master-worker", "Mode haproxy is expected to be running (one of: daemon, master-worker)")
	flag.BoolVar(&showVersion, "version", false, "Show version")
	flag.Parse()

	if showVersion {
		fmt.Println(version)
		os.Exit(0)
	}

	syslog := NewSyslogServer(syslogPort)
	if err := syslog.Start(); err != nil {
		log.Fatalf("Couldn't start embedded syslog: %v\n", err)
	}
	defer syslog.Stop()

	haproxy, err := NewHaproxyServer(haproxyPath, haproxyPIDFile, haproxyConfigFile, haproxyMode)
	if err != nil {
		log.Fatalf("Couldn't start haproxy manager: %v", err)
	}
	if err := haproxy.Start(); err != nil {
		log.Println("Couldn't start haproxy: ", err)
		log.Println("Will wait for valid configuration")
		go func() {
			select {
			case <-watchHaproxyStart(haproxy):
			case <-time.After(configTimeout):
				log.Fatalf("Timeout while waiting for haproxy to start")
			}
		}()
	}
	defer haproxy.Stop()

	done := make(chan os.Signal)
	signal.Notify(done, syscall.SIGTERM, syscall.SIGINT)

	validator := NewHaproxyDashC(haproxyPath, haproxyConfigFile)
	controller := NewController(controlAddress, haproxy, validator)

	go func() {
		for {
			log.Printf("Signal received: %v\n", <-done)
			if err := controller.Stop(); err != nil {
				log.Fatalf("Couldn't cleanly stop controller: %v", err)
			}
		}
	}()

	if err := controller.Run(); err != nil {
		log.Fatalf("Controller failed: %v\n", err)
	}
}
