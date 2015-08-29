// Copyright 2015 tsuru authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package service

import (
	"encoding/json"
	stderrors "errors"
	"io"
	"net/http"
	"regexp"
	"strconv"

	"github.com/tsuru/tsuru/action"
	"github.com/tsuru/tsuru/app/bind"
	"github.com/tsuru/tsuru/auth"
	"github.com/tsuru/tsuru/db"
	"github.com/tsuru/tsuru/errors"
	"github.com/tsuru/tsuru/log"
	"github.com/tsuru/tsuru/rec"
	"gopkg.in/mgo.v2"
	"gopkg.in/mgo.v2/bson"
)

var (
	ErrServiceInstanceNotFound   = stderrors.New("service instance not found")
	ErrInvalidInstanceName       = stderrors.New("invalid service instance name")
	ErrInstanceNameAlreadyExists = stderrors.New("instance name already exists.")
	ErrAccessNotAllowed          = stderrors.New("user does not have access to this service instance")
	ErrMultipleTeams             = stderrors.New("user is member of multiple teams, please specify the team that owns the service instance")
	ErrUnitAlreadyBound          = stderrors.New("unit is already bound to this service instance")
	ErrUnitNotBound              = stderrors.New("unit is not bound to this service instance")

	instanceNameRegexp = regexp.MustCompile(`^[A-Za-z][-a-zA-Z0-9_]+$`)
)

type ServiceInstance struct {
	Name        string
	Id          int
	ServiceName string `bson:"service_name"`
	PlanName    string `bson:"plan_name"`
	Apps        []string
	Units       []string
	Teams       []string
	TeamOwner   string
}

// DeleteInstance deletes the service instance from the database.
func DeleteInstance(si *ServiceInstance) error {
	if len(si.Apps) > 0 {
		msg := "This service instance is bound to at least one app. Unbind them before removing it"
		return stderrors.New(msg)
	}
	endpoint, err := si.Service().getClient("production")
	if err == nil {
		endpoint.Destroy(si)
	}
	conn, err := db.Conn()
	if err != nil {
		return err
	}
	defer conn.Close()
	return conn.ServiceInstances().Remove(bson.M{"name": si.Name})
}

func (si *ServiceInstance) GetIdentifier() string {
	if si.Id != 0 {
		return strconv.Itoa(si.Id)
	}
	return si.Name
}

// MarshalJSON marshals the ServiceName in json format.
func (si *ServiceInstance) MarshalJSON() ([]byte, error) {
	info, err := si.Info()
	if err != nil {
		info = nil
	}
	data := map[string]interface{}{
		"Id":          si.Id,
		"Name":        si.Name,
		"Teams":       si.Teams,
		"Apps":        si.Apps,
		"ServiceName": si.ServiceName,
		"Info":        info,
	}
	return json.Marshal(&data)
}

func (si *ServiceInstance) Info() (map[string]string, error) {
	endpoint, err := si.Service().getClient("production")
	if err != nil {
		return nil, stderrors.New("endpoint does not exists")
	}
	result, err := endpoint.Info(si)
	if err != nil {
		return nil, err
	}
	info := map[string]string{}
	for _, d := range result {
		info[d["label"]] = d["value"]
	}
	return info, nil
}

func (si *ServiceInstance) Create() error {
	conn, err := db.Conn()
	if err != nil {
		return err
	}
	defer conn.Close()
	return conn.ServiceInstances().Insert(si)
}

func (si *ServiceInstance) Service() *Service {
	conn, err := db.Conn()
	if err != nil {
		log.Errorf("Failed to connect to the database: %s", err)
		return nil
	}
	defer conn.Close()
	var s Service
	conn.Services().Find(bson.M{"_id": si.ServiceName}).One(&s)
	return &s
}

func (si *ServiceInstance) FindApp(appName string) int {
	index := -1
	for i, name := range si.Apps {
		if name == appName {
			index = i
			break
		}
	}
	return index
}

func (si *ServiceInstance) update(update bson.M) error {
	conn, err := db.Conn()
	if err != nil {
		return err
	}
	defer conn.Close()
	return conn.ServiceInstances().Update(bson.M{"name": si.Name}, update)
}

// BindApp makes the bind between the service instance and an app.
func (si *ServiceInstance) BindApp(app bind.App, writer io.Writer) error {
	args := bindPipelineArgs{
		serviceInstance: si,
		app:             app,
		writer:          writer,
	}
	actions := []*action.Action{
		&bindAppDBAction,
		&bindAppEndpointAction,
		&setBindedEnvsAction,
		&bindUnitsAction,
	}
	pipeline := action.NewPipeline(actions...)
	return pipeline.Execute(&args)
}

// BindUnit makes the bind between the binder and an unit.
func (si *ServiceInstance) BindUnit(app bind.App, unit bind.Unit) error {
	endpoint, err := si.Service().getClient("production")
	if err != nil {
		return err
	}
	conn, err := db.Conn()
	if err != nil {
		return err
	}
	defer conn.Close()
	updateOp := bson.M{"$addToSet": bson.M{"units": unit.GetName()}}
	err = conn.ServiceInstances().Update(bson.M{"name": si.Name, "units": bson.M{"$ne": unit.GetName()}}, updateOp)
	if err != nil {
		if err == mgo.ErrNotFound {
			return ErrUnitAlreadyBound
		}
		return err
	}
	err = endpoint.BindUnit(si, app, unit)
	if err != nil {
		rollbackErr := si.update(bson.M{"$pull": bson.M{"units": unit.GetName()}})
		if rollbackErr != nil {
			log.Errorf("[bind unit] could remove stil unbinded unit from db after failure: %s", rollbackErr)
		}
		return err
	}
	return nil
}

// UnbindApp makes the unbind between the service instance and an app.
func (si *ServiceInstance) UnbindApp(app bind.App, writer io.Writer) error {
	if si.FindApp(app.GetName()) == -1 {
		return &errors.HTTP{Code: http.StatusPreconditionFailed, Message: "This app is not bound to this service instance."}
	}
	args := bindPipelineArgs{
		serviceInstance: si,
		app:             app,
		writer:          writer,
	}
	actions := []*action.Action{
		&unbindUnits,
		&unbindAppDB,
		&unbindAppEndpoint,
		&removeBindedEnvs,
	}
	pipeline := action.NewPipeline(actions...)
	return pipeline.Execute(&args)
}

// UnbindUnit makes the unbind between the service instance and an unit.
func (si *ServiceInstance) UnbindUnit(app bind.App, unit bind.Unit) error {
	endpoint, err := si.Service().getClient("production")
	if err != nil {
		return err
	}
	conn, err := db.Conn()
	if err != nil {
		return err
	}
	defer conn.Close()
	updateOp := bson.M{"$pull": bson.M{"units": unit.GetName()}}
	err = conn.ServiceInstances().Update(bson.M{"name": si.Name, "units": unit.GetName()}, updateOp)
	if err != nil {
		if err == mgo.ErrNotFound {
			return ErrUnitNotBound
		}
		return err
	}
	err = endpoint.UnbindUnit(si, app, unit)
	if err != nil {
		rollbackErr := si.update(bson.M{"$addToSet": bson.M{"units": unit.GetName()}})
		if rollbackErr != nil {
			log.Errorf("[unbind unit] could not add binded unit back to db after failure: %s", rollbackErr)
		}
		return err
	}
	return nil
}

// Status returns the service instance status.
func (si *ServiceInstance) Status() (string, error) {
	endpoint, err := si.Service().getClient("production")
	if err != nil {
		return "", err
	}
	return endpoint.Status(si)
}

func (si *ServiceInstance) Grant(teamName string) error {
	team, err := auth.GetTeam(teamName)
	if err != nil {
		return err
	}
	return si.update(bson.M{"$push": bson.M{"teams": team.Name}})
}

func (si *ServiceInstance) Revoke(teamName string) error {
	team, err := auth.GetTeam(teamName)
	if err != nil {
		return err
	}
	return si.update(bson.M{"$pull": bson.M{"teams": team.Name}})
}

func genericServiceInstancesFilter(services interface{}, teams []string) bson.M {
	query := bson.M{}
	if len(teams) != 0 {
		query["teams"] = bson.M{"$in": teams}
	}
	if v, ok := services.([]Service); ok {
		names := GetServicesNames(v)
		query["service_name"] = bson.M{"$in": names}
	}
	if v, ok := services.(Service); ok {
		query["service_name"] = v.Name
	}
	return query
}

func validateServiceInstanceName(name string) error {
	if !instanceNameRegexp.MatchString(name) {
		return ErrInvalidInstanceName
	}
	conn, err := db.Conn()
	if err != nil {
		return nil
	}
	defer conn.Close()
	length, err := conn.ServiceInstances().Find(bson.M{"name": name}).Count()
	if length > 0 {
		return ErrInstanceNameAlreadyExists
	}
	return nil
}

func CreateServiceInstance(instance ServiceInstance, service *Service, user *auth.User) error {
	err := validateServiceInstanceName(instance.Name)
	if err != nil {
		return err
	}
	instance.ServiceName = service.Name
	teams, err := user.Teams()
	if err != nil {
		return err
	}
	instance.Teams = make([]string, 0, len(teams))
	for _, team := range teams {
		if service.HasTeam(&team) || !service.IsRestricted {
			instance.Teams = append(instance.Teams, team.Name)
		}
	}
	if instance.TeamOwner == "" {
		if len(instance.Teams) > 1 {
			return ErrMultipleTeams
		}
		instance.TeamOwner = instance.Teams[0]
	} else {
		var found bool
		for _, team := range instance.Teams {
			if instance.TeamOwner == team {
				found = true
				break
			}
		}
		if !found {
			return auth.ErrTeamNotFound
		}
	}
	actions := []*action.Action{&createServiceInstance, &insertServiceInstance}
	pipeline := action.NewPipeline(actions...)
	return pipeline.Execute(*service, instance, user.Email)
}

func GetServiceInstancesByServices(services []Service) ([]ServiceInstance, error) {
	var instances []ServiceInstance
	conn, err := db.Conn()
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	query := genericServiceInstancesFilter(services, []string{})
	f := bson.M{"name": 1, "service_name": 1}
	err = conn.ServiceInstances().Find(query).Select(f).All(&instances)
	return instances, err
}

func GetServiceInstancesByServicesAndTeams(services []Service, u *auth.User, appName string) ([]ServiceInstance, error) {
	var instances []ServiceInstance
	teams, err := u.Teams()
	if err != nil {
		return nil, err
	}
	if len(teams) == 0 {
		return nil, nil
	}
	conn, err := db.Conn()
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	var teamNames []string
	if !u.IsAdmin() {
		teamNames = auth.GetTeamsNames(teams)
	}
	query := genericServiceInstancesFilter(services, teamNames)
	if appName != "" {
		query["apps"] = appName
	}
	err = conn.ServiceInstances().Find(query).All(&instances)
	return instances, err
}

func GetServiceInstance(name string, u *auth.User) (*ServiceInstance, error) {
	conn, err := db.Conn()
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	rec.Log(u.Email, "get-service-instance", name)
	var instance ServiceInstance
	err = conn.ServiceInstances().Find(bson.M{"name": name}).One(&instance)
	if err != nil {
		return nil, ErrServiceInstanceNotFound
	}
	if !auth.CheckUserAccess(instance.Teams, u) {
		return nil, ErrAccessNotAllowed
	}
	return &instance, nil
}

// Proxy is a proxy between tsuru and the service.
// This method allow customized service methods.
func Proxy(si *ServiceInstance, path string, w http.ResponseWriter, r *http.Request) error {
	endpoint, err := si.Service().getClient("production")
	if err != nil {
		return err
	}
	return endpoint.Proxy(path, w, r)
}
