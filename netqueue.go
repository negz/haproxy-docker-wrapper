// Copyright © 2017 Tuenti Technologies S.L.
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
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"

	nfqueue "github.com/AkihiroSuda/go-netfilter-queue"
)

const maxPacketsInQueue = 65536

const packetTimeout = 100 * time.Millisecond

const iptablesAddFlag = "-A"
const iptablesDeleteFlag = "-D"

const procNetfilterQueuePath = "/proc/net/netfilter/nfnetlink_queue"

var netQueue NetQueue

func ipArgs(arg string) ([]net.IP, error) {
	if len(arg) == 0 {
		return nil, nil
	}
	ipArgs := strings.Split(arg, ",")
	ips := make([]net.IP, len(ipArgs))
	for i := range ipArgs {
		ip := net.ParseIP(ipArgs[i])
		if ip == nil {
			return nil, fmt.Errorf("incorrect IP: %s", ipArgs[i])
		}
		ips[i] = ip
	}
	return ips, nil
}

// A NetQueue retains new connections while haproxy is reloaded
type NetQueue interface {
	Capture()
	Release()
}

type NetfilterQueue struct {
	Number uint
	IPs    []net.IP

	capture, capturing, release chan struct{}
}

func NewNetfilterQueue(n uint, ips []net.IP) *NetfilterQueue {
	q := NetfilterQueue{
		Number:    n,
		IPs:       ips,
		capture:   make(chan struct{}, 1),
		capturing: make(chan struct{}, 1),
		release:   make(chan struct{}, 1),
	}
	go q.loop()
	return &q
}

func (q *NetfilterQueue) iptables(flag string) {
	for _, ip := range q.IPs {
		if ip.To4() == nil {
			log.Println("Only IPv4 addresses supported: %s found", ip.String())
			continue
		}
		args := []string{
			flag,
			"INPUT", "-j", "NFQUEUE", "-w",
			"-p", "tcp", "--syn", "--destination", ip.String(),
			"--queue-num", strconv.Itoa(int(q.Number)),
		}

		err := exec.Command("iptables", args...).Run()
		if err != nil {
			panic(fmt.Sprintf("iptables failed: %v", err))
		}
	}
}

func (q *NetfilterQueue) loop() {
	if len(q.IPs) == 0 {
		return
	}
	queue, err := nfqueue.NewNFQueue(uint16(q.Number), maxPacketsInQueue, nfqueue.NF_DEFAULT_PACKET_SIZE)
	if err != nil {
		panic(err)
	}
	defer queue.Close()

	accepting := true
	accept := sync.NewCond(&sync.Mutex{})
	accept.L.Lock()
	go func() {
		count := 0
		for {
			select {
			case packet := <-queue.GetPackets():
				for !accepting {
					accept.Wait()
				}
				count++
				packet.SetVerdict(nfqueue.NF_ACCEPT)
			case <-time.After(packetTimeout):
				if count > 0 {
					log.Printf("Delayed %d packages during reloads\n", count)
					count = 0
				}
			}
		}
	}()

	for {
		<-q.capture
		accepting = false
		func() {
			q.iptables(iptablesAddFlag)
			defer q.iptables(iptablesDeleteFlag)
			q.capturing <- struct{}{}
			<-q.release
		}()
		accepting = true
		accept.Signal()
	}
}

func (q *NetfilterQueue) Capture() {
	if len(q.IPs) == 0 {
		return
	}
	q.capture <- struct{}{}
	<-q.capturing
}

func (q *NetfilterQueue) Release() {
	if len(q.IPs) == 0 {
		return
	}
	q.release <- struct{}{}
}

type ProcNetfilterQueue struct {
	ID           uint
	PortID       uint
	Waiting      uint
	CopyMode     uint
	CopyRange    uint
	QueueDropped uint
	UserDropped  uint
	LastSeq      uint
	One          uint
}

type ProcNetfilter struct {
	sync.RWMutex

	queues map[uint]ProcNetfilterQueue
}

func (pn *ProcNetfilter) Get(id uint) (ProcNetfilterQueue, bool) {
	pn.RLock()
	defer pn.RUnlock()

	q, found := pn.queues[id]
	return q, found
}

func (pn *ProcNetfilter) Update() error {
	pn.Lock()
	defer pn.Unlock()

	f, err := os.Open(procNetfilterQueuePath)
	if err != nil {
		return err
	}
	defer f.Close()

	seen := make(map[uint]bool)

	var id, portID, waiting, copyMode, copyRange, queueDropped, userDropped, lastSeq, one uint
	for {
		_, err := fmt.Fscanf(f, "%d %d %d %d %d %d %d %d %d\n",
			&id, &portID, &waiting, &copyMode, &copyRange, &queueDropped, &userDropped, &lastSeq, &one)
		seen[id] = true
		pn.queues[id] = ProcNetfilterQueue{
			ID:           id,
			PortID:       portID,
			Waiting:      waiting,
			CopyMode:     copyMode,
			CopyRange:    copyRange,
			QueueDropped: queueDropped,
			UserDropped:  userDropped,
			LastSeq:      lastSeq,
			One:          one,
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
	}
	for k := range pn.queues {
		if _, found := seen[k]; !found {
			delete(pn.queues, k)
		}
	}
	return nil
}

func ReadProcNetfilter() (*ProcNetfilter, error) {
	pn := &ProcNetfilter{queues: make(map[uint]ProcNetfilterQueue)}
	err := pn.Update()
	if err != nil {
		return nil, err
	}
	return pn, nil
}
