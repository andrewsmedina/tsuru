// Copyright 2015 tsuru authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package galeb

import (
	"fmt"
	"net"
	"net/url"
	"strconv"

	"github.com/tsuru/config"
	"github.com/tsuru/tsuru/db"
	"github.com/tsuru/tsuru/db/storage"
	"github.com/tsuru/tsuru/router"
	galebClient "github.com/tsuru/tsuru/router/galeb/client"
)

const routerName = "galeb"

type galebRouter struct {
	client *galebClient.GalebClient
	domain string
	prefix string
}

func init() {
	router.Register(routerName, createRouter)
}

func createRouter(prefix string) (router.Router, error) {
	apiUrl, err := config.GetString(prefix + ":api-url")
	if err != nil {
		return nil, err
	}
	username, err := config.GetString(prefix + ":username")
	if err != nil {
		return nil, err
	}
	password, err := config.GetString(prefix + ":password")
	if err != nil {
		return nil, err
	}
	domain, err := config.GetString(prefix + ":domain")
	if err != nil {
		return nil, err
	}
	environment, _ := config.GetString(prefix + ":environment")
	farmType, _ := config.GetString(prefix + ":farm-type")
	plan, _ := config.GetString(prefix + ":plan")
	project, _ := config.GetString(prefix + ":project")
	loadBalancePolicy, _ := config.GetString(prefix + ":load-balance-policy")
	ruleType, _ := config.GetString(prefix + ":rule-type")
	client := galebClient.GalebClient{
		ApiUrl:            apiUrl,
		Username:          username,
		Password:          password,
		Environment:       environment,
		FarmType:          farmType,
		Plan:              plan,
		Project:           project,
		LoadBalancePolicy: loadBalancePolicy,
		RuleType:          ruleType,
	}
	r := galebRouter{
		client: &client,
		domain: domain,
		prefix: prefix,
	}
	return &r, nil
}

func collection() (*storage.Collection, error) {
	conn, err := db.Conn()
	if err != nil {
		return nil, err
	}
	return conn.Collection("galeb_router"), nil
}

func poolName(base string) string {
	return fmt.Sprintf("tsuru-backendpool-%s", base)
}

func rootRuleName(base string) string {
	return fmt.Sprintf("tsuru-rootrule-%s", base)
}

func (r *galebRouter) virtualHostName(base string) string {
	return fmt.Sprintf("%s.%s", base, r.domain)
}

func (r *galebRouter) getClient() (*galebClient.GalebClient, error) {
	return r.client, nil
}

func (r *galebRouter) AddBackend(name string) error {
	poolParams := galebClient.BackendPoolParams{
		Name: poolName(name),
	}
	_, err := getGalebData(name)
	if err == nil {
		return router.ErrBackendExists
	}
	data := galebData{Name: name}
	client, err := r.getClient()
	if err != nil {
		return err
	}
	data.BackendPoolId, err = client.AddBackendPool(&poolParams)
	if err != nil {
		return err
	}
	ruleParams := galebClient.RuleParams{
		Name:        rootRuleName(name),
		Match:       "/",
		BackendPool: data.BackendPoolId,
	}
	data.RootRuleId, err = client.AddRule(&ruleParams)
	if err != nil {
		return err
	}
	virtualHostParams := galebClient.VirtualHostParams{
		Name:        r.virtualHostName(name),
		RuleDefault: data.RootRuleId,
	}
	data.VirtualHostId, err = client.AddVirtualHost(&virtualHostParams)
	if err != nil {
		return err
	}
	err = data.save()
	if err != nil {
		return err
	}
	return router.Store(name, name, routerName)
}

func (r *galebRouter) RemoveBackend(name string) error {
	backendName, err := router.Retrieve(name)
	if err != nil {
		return err
	}
	if backendName != name {
		return router.ErrBackendSwapped
	}
	data, err := getGalebData(backendName)
	if err != nil {
		return err
	}
	client, err := r.getClient()
	if err != nil {
		return err
	}
	err = client.RemoveResource(data.VirtualHostId)
	if err != nil {
		return err
	}
	for _, cnameData := range data.CNames {
		err = client.RemoveResource(cnameData.VirtualHostId)
		if err != nil {
			return err
		}
	}
	err = client.RemoveResource(data.RootRuleId)
	if err != nil {
		return err
	}
	err = client.RemoveResource(data.BackendPoolId)
	if err != nil {
		return err
	}
	err = data.remove()
	if err != nil {
		return err
	}
	return router.Remove(backendName)
}

func (r *galebRouter) AddRoute(name string, address *url.URL) error {
	backendName, err := router.Retrieve(name)
	if err != nil {
		return err
	}
	data, err := getGalebData(backendName)
	if err != nil {
		return err
	}
	for _, r := range data.Reals {
		if r.Real == address.Host {
			return router.ErrRouteExists
		}
	}
	client, err := r.getClient()
	if err != nil {
		return err
	}
	host, portStr, _ := net.SplitHostPort(address.Host)
	port, _ := strconv.Atoi(portStr)
	params := galebClient.BackendParams{
		Ip:          host,
		Port:        port,
		BackendPool: data.BackendPoolId,
	}
	backendId, err := client.AddBackend(&params)
	if err != nil {
		return err
	}
	return data.addReal(address.Host, backendId)
}

func (r *galebRouter) RemoveRoute(name string, address *url.URL) error {
	backendName, err := router.Retrieve(name)
	if err != nil {
		return err
	}
	data, err := getGalebData(backendName)
	if err != nil {
		return err
	}
	client, err := r.getClient()
	if err != nil {
		return err
	}
	for _, real := range data.Reals {
		if real.Real == address.Host {
			err = client.RemoveResource(real.BackendId)
			if err != nil {
				return err
			}
			return data.removeReal(address.Host)
		}
	}
	return router.ErrRouteNotFound
}

func (r *galebRouter) SetCName(cname, name string) error {
	backendName, err := router.Retrieve(name)
	if err != nil {
		return err
	}
	domain, err := config.GetString(r.prefix + ":domain")
	if err != nil {
		return err
	}
	if !router.ValidCName(cname, domain) {
		return router.ErrCNameNotAllowed
	}
	data, err := getGalebData(backendName)
	if err != nil {
		return err
	}
	client, err := r.getClient()
	if err != nil {
		return err
	}
	for _, val := range data.CNames {
		if val.CName == cname {
			return router.ErrCNameExists
		}
	}
	virtualHostParams := galebClient.VirtualHostParams{
		Name:        cname,
		RuleDefault: data.RootRuleId,
	}
	virtualHostId, err := client.AddVirtualHost(&virtualHostParams)
	if err != nil {
		return err
	}
	return data.addCName(cname, virtualHostId)
}

func (r *galebRouter) UnsetCName(cname, name string) error {
	backendName, err := router.Retrieve(name)
	if err != nil {
		return err
	}
	data, err := getGalebData(backendName)
	if err != nil {
		return err
	}
	client, err := r.getClient()
	if err != nil {
		return err
	}
	for _, cnameData := range data.CNames {
		if cnameData.CName == cname {
			err = client.RemoveResource(cnameData.VirtualHostId)
			if err != nil {
				return err
			}
			return data.removeCName(cname)
		}
	}
	return router.ErrCNameNotFound
}

func (r *galebRouter) Addr(name string) (string, error) {
	backendName, err := router.Retrieve(name)
	if err != nil {
		return "", err
	}
	_, err = getGalebData(backendName)
	if err != nil {
		return "", err
	}
	return r.virtualHostName(backendName), nil
}

func (r *galebRouter) Swap(backend1, backend2 string) error {
	return router.Swap(r, backend1, backend2)
}

func (r *galebRouter) Routes(name string) ([]*url.URL, error) {
	backendName, err := router.Retrieve(name)
	if err != nil {
		return nil, err
	}
	data, err := getGalebData(backendName)
	if err != nil {
		return nil, err
	}
	result := make([]*url.URL, len(data.Reals))
	for i, real := range data.Reals {
		var url url.URL
		url.Scheme = "http"
		url.Host = real.Real
		result[i] = &url
	}
	return result, nil
}

func (r galebRouter) StartupMessage() (string, error) {
	domain, err := config.GetString(r.prefix + ":domain")
	if err != nil {
		return "", err
	}
	apiUrl, err := config.GetString(r.prefix + ":api-url")
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("galeb router %q with API URL %q.", domain, apiUrl), nil
}
