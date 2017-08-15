/**
 * Licensed to the Apache Software Foundation (ASF) under one
 * or more contributor license agreements.  See the NOTICE file
 * distributed with this work for additional information
 * regarding copyright ownership.  The ASF licenses this file
 * to you under the Apache License, Version 2.0 (the
 * "License"); you may not use this file except in compliance
 * with the License.  You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

package rpc

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net"
	"sort"
	"strconv"
	"strings"
	"time"
	"unsafe"

	"github.com/gogo/protobuf/proto"
	log "github.com/golang/glog"
	mesos "github.com/mesos/mesos-go/mesosproto"
	"github.com/samuel/go-zookeeper/zk"
	"github.com/adobe-platform/zk-cli/cli"
	"crypto/sha1"
	"encoding/base64"
)

func ConvertCreds(acls [] zk.ACL) []zk.ACL {
	aa := make([] zk.ACL, 0)
	for _, acl := range acls {
		if acl.Scheme == "digest" {
			h := sha1.New()
			if _, err := h.Write([]byte(acl.ID)); err != nil {
				panic("SHA1 failed")
			}
			digest := base64.StdEncoding.EncodeToString(h.Sum(nil))
			items := strings.Split(acl.ID, ":")
			aa = append(aa, zk.ACL{Scheme: "digest", Perms: acl.Perms, ID: fmt.Sprintf("%s:%s", items[0], digest) })
		} else {
			aa = append(aa, acl)
		}

	}
	return aa
}
//ZConn -- encapsulate connection adding perms
type ZConn struct {
	*zk.Conn
	Ev <-chan zk.Event
}
// NewConnection
func NewConnection(zkServers []string, zkAcls []zk.ACL) (conn *ZConn, err error) {
	conn = &ZConn{}

	conn.Conn, conn.Ev, err = zk.Connect(zkServers, RPC_TIMEOUT)
	if err != nil {
		return nil, err
	}
	for _, acl := range zkAcls {
		conn.Conn.AddAuth(acl.Scheme, []byte(acl.ID))
	}
	return conn, nil
}
// WithTransformedACLs - takes the cli passed acls and digests them for use in setting acls
func (conn *ZConn) WithTransformedACLs(raw []zk.ACL, ctxt func(acls []zk.ACL) error) error {
	if ctxt != nil {
		temp := ConvertCreds(raw)
		if temp == nil || len(temp) == 0 {
			temp = zk.WorldACL(zk.PermAll)
		}

		return ctxt(temp)
	}
	return errors.New("Missing function ")
}

func PersistFrameworkID(fwid *mesos.FrameworkID, zkServers []string, zkChroot string, zkAcls []zk.ACL, frameworkName string) error {
	c, err := NewConnection(zkServers, zkAcls)
	if err != nil {
		return err
	}
	defer c.Close()

	c.WithTransformedACLs(zkAcls, func(transformedACLs []zk.ACL) error {
		log.Infof("PersistFrameworkID %s acls: %+v", zkChroot, transformedACLs)
		// attempt to create the path
		_, err = c.Create(
			zkChroot,
			[]byte(""),
			0,
			transformedACLs,
		)
		if err != nil && err != zk.ErrNodeExists {
			return err
		}
		// attempt to write framework ID to <path> / <frameworkName>
		_, err = c.Create(zkChroot + "/" + frameworkName + "_framework_id",
			[]byte(fwid.GetValue()),
			0,
			transformedACLs)
		// TODO(tyler) when err is zk.ErrNodeExists, cross-check value
		if err != nil {
			return err
		}
		log.Info("Successfully persisted Framework ID to zookeeper.")

		return nil
	})

	return nil
}

func UpdateReconciliationInfo(reconciliationInfo map[string]string, zkServers []string, zkChroot string, zkAcls []zk.ACL, frameworkName string) error {
	serializedReconciliationInfo, err := json.Marshal(reconciliationInfo)

	c, err := NewConnection(zkServers, zkAcls)
	if err != nil {
		return err
	}
	defer c.Close()

	var outerErr error = nil
	backoff := 1
	log.Info("persisting reconciliation info to zookeeper")
	// Use extra retries here because we really don't want to fall out of
	// sync here.
	for retries := 0; retries < RPC_RETRIES * 2; retries++ {
		outerErr = c.WithTransformedACLs(zkAcls, func(transformedACLs []zk.ACL) (err error) {
			path := zkChroot + "/" + frameworkName + "_reconciliation"

			log.V(3).Infof("UpdateReconciliationInfo %s\n", path)

			// try to update an existing node, which may fail if it
			// does not exist yet.
			_, err = c.Set(path,
				serializedReconciliationInfo,
				-1)
			if err != zk.ErrNoNode {
				return err
			}

			// attempt to create the node, as it does not exist
			_, err = c.Create(path,
				serializedReconciliationInfo,
				0,
				transformedACLs,
			)
			if err != nil {
				return err
			}
			log.Info("Successfully persisted reconciliation info to zookeeper.")

			return nil
		})
		if outerErr == nil {
			break
		}
		log.Warningf("Failed to configure cluster for new instance: %+v.  " +
		"Backing off for %d seconds and retrying.", outerErr, backoff)
		time.Sleep(time.Duration(backoff) * time.Second)
		backoff = int(math.Min(float64(backoff << 1), 8))
	}

	return outerErr
}

func GetPreviousFrameworkID(zkServers []string, zkChroot string, zkAcls []zk.ACL, frameworkName string, ) (fwid string, err error) {
	c, err := NewConnection(zkServers, zkAcls)
	if err != nil {
		return "",err
	}
	defer c.Close()

	backoff := 1
	for retries := 0; retries < RPC_RETRIES; retries++ {
		if err = c.WithTransformedACLs(zkAcls, func(transformedACLs []zk.ACL) (err error) {
			path := zkChroot + "/" + frameworkName + "_framework_id"
			log.V(3).Infof("GetPreviousFrameworkID %s\n", path)
			rawData, _, err2 := c.Get(path)
			fwid = string(rawData)
			return err2
		}); err == nil {
			return fwid, err
		}
		time.Sleep(time.Duration(backoff) * time.Second)
		backoff = int(math.Min(float64(backoff << 1), 8))
	}
	return "", err
}

func GetPreviousReconciliationInfo(zkServers []string, zkChroot string, zkAcls []zk.ACL, frameworkName string, ) (recon map[string]string, err error) {
	c, err := NewConnection(zkServers, zkAcls)
	if err != nil {
		return map[string]string{}, err
	}
	defer c.Close()

	backoff := 1
	for retries := 0; retries < RPC_RETRIES; retries++ {
		if err = c.WithTransformedACLs(zkAcls, func(transformedACLs []zk.ACL) (err error) {
			path := zkChroot + "/" + frameworkName + "_reconciliation"
			log.Infof("GetPreviousReconciliationInfo %s\n", path)
			rawData, _, err := c.Get(path)
			if err != nil {
				return err
			}
			recon := map[string]string{}
			err = json.Unmarshal(rawData, &recon)
			if err != nil {
				return err
			}
			return nil
		}); err != nil {
			if err == zk.ErrNodeExists {
				return map[string]string{}, err
			}
			time.Sleep(time.Duration(backoff) * time.Second)
			backoff = int(math.Min(float64(backoff << 1), 8))
			continue
		}
		return recon, err
	}
	return map[string]string{}, err
}

func ClearZKState(zkServers []string, zkChroot string, zkAcls []zk.ACL, frameworkName string) error {

	c, err := NewConnection(zkServers, zkAcls)
	if err != nil {
		return err
	}
	defer c.Close()

	return c.WithTransformedACLs(zkAcls, func(transformedACLs []zk.ACL) (err error) {

		err1 := c.Delete(zkChroot + "/" + frameworkName + "_framework_id", -1)
		err2 := c.Delete(zkChroot + "/" + frameworkName + "_reconciliation", -1)
		if err1 != nil {
			return err1
		} else if err2 != nil {
			return err2
		} else {
			return nil
		}
	})
}

type decoder func([]byte, interface{}) error

var infoCodecs = map[string]decoder{
	"info_": func(b []byte, i interface{}) error {
		return proto.Unmarshal(b, i.(proto.Message))
	},
	"json.info_": json.Unmarshal,
}

type nodeGetter func(string) ([]byte, error)

func masterInfoFromZKNodes(children []string, ng nodeGetter, codecs map[string]decoder) (*mesos.MasterInfo, string, error) {
	type nodeDecoder struct {
		node    string
		decoder decoder
	}
	// for example: 001 -> {info_001,proto.Unmarshal} or 001 -> {json.info_001,json.Unmarshal}
	mapped := make(map[string]nodeDecoder, len(children))

	// process children deterministically, preferring json to protobuf
	sort.Sort(sort.Reverse(sort.StringSlice(children)))
	childloop:
	for i := range children {
		for p := range codecs {
			if strings.HasPrefix(children[i], p) {
				key := children[i][len(p):]
				if _, found := mapped[key]; found {
					continue childloop
				}
				mapped[key] = nodeDecoder{children[i], codecs[p]}
				children[i] = key
				continue childloop
			}
		}
	}

	if len(mapped) == 0 {
		return nil, "", errors.New("Could not find current mesos master in zk")
	}

	sort.Sort(sort.StringSlice(children))
	var (
		info mesos.MasterInfo
		lowest = children[0]
	)
	rawData, err := ng(mapped[lowest].node)
	if err == nil {
		err = mapped[lowest].decoder(rawData, &info)
	}
	return &info, string(rawData), err
}

// byteOrder is instantiated at package initialization time to the
// binary.ByteOrder of the running process.
// https://groups.google.com/d/msg/golang-nuts/zmh64YkqOV8/iJe-TrTTeREJ
var byteOrder = func() binary.ByteOrder {
	switch x := uint32(0x01020304); *(*byte)(unsafe.Pointer(&x)) {
	case 0x01:
		return binary.BigEndian
	case 0x04:
		return binary.LittleEndian
	}
	panic("unknown byte order")
}()

func addressFrom(info *mesos.MasterInfo) string {
	var (
		host string
		port int
	)
	if addr := info.GetAddress(); addr != nil {
		host = addr.GetHostname()
		if host == "" {
			host = addr.GetIp()
		}
		port = int(addr.GetPort())
	}
	if host == "" {
		host = info.GetHostname()
		if host == "" {
			if ipAsInt := info.GetIp(); ipAsInt != 0 {
				ip := make([]byte, 4)
				byteOrder.PutUint32(ip, ipAsInt)
				host = net.IP(ip).To4().String()
			}
		}
		port = int(info.GetPort())
	}
	if host == "" {
		return ""
	}
	return net.JoinHostPort(host, strconv.Itoa(port))
}

func addressFromRaw(rawData string) (string, error) {
	// scrape the contents of the raw buffer for anything that looks like a PID
	var (
		mraw = strings.Split(string(rawData), "@")[1]
		mraw2 = strings.Split(mraw, ":")
		host = mraw2[0]
		port = 0
		_, err = fmt.Sscanf(mraw2[1], "%d", &port)
	)
	if err != nil {
		return "", err
	}
	return net.JoinHostPort(host, strconv.Itoa(port)), nil
}


// need master
func GetMasterFromZK(zkURI string) (  string, error) {
	servers, chroot, acls, err := cli.ParseZKURI(zkURI)
	if err != nil {
		return "", nil
	}

	c, err := NewConnection(servers, acls)
	if err != nil {
		return "", err
	}
	defer c.Close()
	children, _, err := c.Children(chroot)
	if err != nil {
		return "", err
	}
	getter := func(node string) (rawData []byte, err error) {
		rawData, _, err = c.Get(chroot + "/" + node)
		return
	}
	info, rawData, err := masterInfoFromZKNodes(children, getter, infoCodecs)
	if err != nil {
		return "", err
	}
	addr := addressFrom(info)
	if addr == "" {
		addr, err = addressFromRaw(rawData)
	}
	return addr, err
}
