// Copyright 2015 tsuru authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package docker

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"

	"github.com/tsuru/docker-cluster/cluster"
	"github.com/tsuru/tsuru/app"
	"github.com/tsuru/tsuru/provision"
	"github.com/tsuru/tsuru/provision/docker/container"
	"github.com/tsuru/tsuru/provision/provisiontest"
	"gopkg.in/check.v1"
	"gopkg.in/mgo.v2/bson"
)

func startDocker(hostPort string) (func(), *httptest.Server, *dockerProvisioner) {
	output := `{
    "State": {
        "Running": true,
        "Pid": 2785,
        "ExitCode": 0,
        "StartedAt": "2013-08-15T03:38:45.709874216-03:00",
        "Ghost": false
    },
    "Image": "b750fe79269d2ec9a3c593ef05b4332b1d1a02a62b4accb2c21d589ff2f5f2dc",
	"NetworkSettings": {
		"IpAddress": "127.0.0.9",
		"IpPrefixLen": 8,
		"Gateway": "10.65.41.1",
		"Ports": {
			"8888/tcp": [
				{
					"HostIp": "0.0.0.0",
					"HostPort": "%s"
				}
			]
		}
	}
}`
	output = fmt.Sprintf(output, hostPort)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/containers/9930c24f1c4x") {
			w.Write([]byte(output))
		}
	}))
	var err error
	var p dockerProvisioner
	err = p.Initialize()
	if err != nil {
		panic(err)
	}
	p.cluster, err = cluster.New(nil, &cluster.MapStorage{},
		cluster.Node{Address: server.URL},
	)
	if err != nil {
		panic(err)
	}
	return func() {
		server.Close()
	}, server, &p
}

func (s *S) TestFixContainers(c *check.C) {
	cleanup, server, p := startDocker("9999")
	defer cleanup()
	coll := p.Collection()
	defer coll.Close()
	err := coll.Insert(
		container.Container{
			ID:       "9930c24f1c4x",
			AppName:  "makea",
			Type:     "python",
			Status:   provision.StatusStarted.String(),
			IP:       "127.0.0.4",
			HostPort: "9025",
			HostAddr: "127.0.0.1",
		},
	)
	c.Assert(err, check.IsNil)
	defer coll.RemoveAll(bson.M{"appname": "makea"})
	err = s.storage.Apps().Insert(&app.App{Name: "makea"})
	c.Assert(err, check.IsNil)
	appInstance := provisiontest.NewFakeApp("makea", "python", 0)
	defer p.Destroy(appInstance)
	p.Provision(appInstance)
	var storage cluster.MapStorage
	storage.StoreContainer("9930c24f1c4x", server.URL)
	p.cluster, err = cluster.New(nil, &storage,
		cluster.Node{Address: server.URL},
	)
	c.Assert(err, check.IsNil)
	err = p.fixContainers()
	c.Assert(err, check.IsNil)
	cont, err := p.GetContainer("9930c24f1c4x")
	c.Assert(err, check.IsNil)
	c.Assert(cont.IP, check.Equals, "127.0.0.9")
	c.Assert(cont.HostPort, check.Equals, "9999")
}

func (s *S) TestFixContainersEmptyPortDoesNothing(c *check.C) {
	cleanup, server, p := startDocker("")
	defer cleanup()
	coll := p.Collection()
	defer coll.Close()
	err := coll.Insert(
		container.Container{
			ID:       "9930c24f1c4x",
			AppName:  "makea",
			Type:     "python",
			Status:   provision.StatusStarted.String(),
			IP:       "",
			HostPort: "",
			HostAddr: "127.0.0.1",
		},
	)
	c.Assert(err, check.IsNil)
	defer coll.RemoveAll(bson.M{"appname": "makea"})
	var storage cluster.MapStorage
	storage.StoreContainer("9930c24f1c4x", server.URL)
	p.cluster, err = cluster.New(nil, &storage,
		cluster.Node{Address: server.URL},
	)
	c.Assert(err, check.IsNil)
	err = p.fixContainers()
	c.Assert(err, check.IsNil)
	cont, err := p.GetContainer("9930c24f1c4x")
	c.Assert(err, check.IsNil)
	c.Assert(cont.IP, check.Equals, "")
	c.Assert(cont.HostPort, check.Equals, "")
}
