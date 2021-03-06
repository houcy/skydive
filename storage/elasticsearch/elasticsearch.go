/*
 * Copyright (C) 2015 Red Hat, Inc.
 *
 * Licensed to the Apache Software Foundation (ASF) under one
 * or more contributor license agreements.  See the NOTICE file
 * distributed with this work for additional information
 * regarding copyright ownership.  The ASF licenses this file
 * to you under the Apache License, Version 2.0 (the
 * "License"); you may not use this file except in compliance
 * with the License.  You may obtain a copy of the License at
 *
 *  http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing,
 * software distributed under the License is distributed on an
 * "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY
 * KIND, either express or implied.  See the License for the
 * specific language governing permissions and limitations
 * under the License.
 *
 */

package elasticsearch

import (
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	elastigo "github.com/mattbaird/elastigo/lib"

	"github.com/redhat-cip/skydive/config"
	"github.com/redhat-cip/skydive/flow"
	"github.com/redhat-cip/skydive/logging"
	"github.com/redhat-cip/skydive/storage"
)

const indexVersion = 2

const mapping = `
{"mappings":{"flow":{"dynamic_templates":[
	{"notanalyzed_graph":{"match":"*NodeUUID","mapping":{"type":"string","index":"not_analyzed"}}},
	{"notanalyzed_layers":{"match":"LayersPath","mapping":{"type":"string","index":"not_analyzed"}}},
	{"start_epoch":{"match":"Start","mapping":{"type":"date", "format": "epoch_second"}}},
	{"last_epoch":{"match":"Last","mapping":{"type":"date", "format": "epoch_second"}}}
]}}}
`

type ElasticSearchStorage struct {
	connection *elastigo.Conn
	indexer    *elastigo.BulkIndexer
	started    atomic.Value
}

func (c *ElasticSearchStorage) StoreFlows(flows []*flow.Flow) error {
	if c.started.Load() != true {
		return errors.New("ElasticSearchStorage is not yet started")
	}

	for _, flow := range flows {
		err := c.indexer.Index("skydive", "flow", flow.UUID, "", "", nil, flow)
		if err != nil {
			logging.GetLogger().Errorf("Error while indexing: %s", err.Error())
			continue
		}
	}

	return nil
}

func (c *ElasticSearchStorage) SearchFlows(filters *storage.Filters) ([]*flow.Flow, error) {
	if c.started.Load() != true {
		return nil, errors.New("ElasticSearchStorage is not yet started")
	}

	request := map[string]interface{}{
		"sort": map[string]interface{}{
			"Statistics.Last": map[string]string{
				"order": "desc",
			},
		},
		"from": 0,
		"size": 5,
	}

	if len(filters.Term.Terms)+len(filters.Range) > 0 {
		var musts []interface{}
		var terms []interface{}
		if len(filters.Range) > 0 {
			for k, v := range filters.Range {
				term := map[string]interface{}{
					"range": map[string]interface{}{
						k: v,
					},
				}
				musts = append(musts, term)
			}
		}

		if len(filters.Term.Terms) > 0 {
			for k, v := range filters.Term.Terms {
				term := map[string]interface{}{
					"term": map[string]interface{}{
						k: v,
					},
				}
				terms = append(terms, term)
			}
		}

		op := "and"
		if filters.Term.Op == storage.OR {
			op = "or"
		}

		query := map[string]interface{}{
			"bool": map[string]interface{}{
				"must": musts,
				"filter": map[string]interface{}{
					op: terms,
				},
			},
		}

		request["query"] = query
	}

	q, err := json.Marshal(request)
	if err != nil {
		return nil, err
	}

	out, err := c.connection.Search("skydive", "flow", nil, string(q))
	if err != nil {
		return nil, err
	}

	flows := []*flow.Flow{}

	if out.Hits.Len() > 0 {
		for _, d := range out.Hits.Hits {
			f := new(flow.Flow)
			err := json.Unmarshal([]byte(*d.Source), f)
			if err != nil {
				return nil, err
			}

			flows = append(flows, f)
		}
	}

	return flows, nil
}

func (c *ElasticSearchStorage) request(method string, path string, query string, body string) (int, []byte, error) {
	req, err := c.connection.NewRequest(method, path, query)
	if err != nil {
		return 503, nil, err
	}

	if body != "" {
		req.SetBodyString(body)
	}

	var response map[string]interface{}
	return req.Do(&response)
}

func (c *ElasticSearchStorage) initialize() error {
	indexPath := fmt.Sprintf("/skydive_v%d", indexVersion)

	code, _, _ := c.request("GET", indexPath, "", "")
	if code == 200 {
		return nil
	}

	code, _, _ = c.request("PUT", indexPath, "", mapping)
	if code != 200 {
		return errors.New("Unable to create the skydive index: " + strconv.FormatInt(int64(code), 10))
	}

	aliases := `{"actions": [`

	code, data, _ := c.request("GET", "/_aliases", "", "")
	if code == 200 {
		var current map[string]interface{}

		err := json.Unmarshal(data, &current)
		if err != nil {
			return errors.New("Unable to parse aliases: " + err.Error())
		}

		for k := range current {
			if strings.HasPrefix(k, "skydive_") {
				remove := `{"remove":{"alias": "skydive", "index": "%s"}},`
				aliases += fmt.Sprintf(remove, k)
			}
		}
	}

	add := `{"add":{"alias": "skydive", "index": "skydive_v%d"}}]}`
	aliases += fmt.Sprintf(add, indexVersion)

	code, _, _ = c.request("POST", "/_aliases", "", aliases)
	if code != 200 {
		return errors.New("Unable to create an alias to the skydive index: " + strconv.FormatInt(int64(code), 10))
	}

	logging.GetLogger().Infof("ElasticSearchStorage started")

	return nil
}

var ErrBadConfig = errors.New("elasticsearch : Config file is misconfigured, check elasticsearch key format")

func (c *ElasticSearchStorage) start() {
	for {
		err := c.initialize()
		if err == nil {
			break
		}
		logging.GetLogger().Errorf("Unable to get connected to Elasticsearch: %s", err.Error())

		time.Sleep(1 * time.Second)
	}

	c.indexer = c.connection.NewBulkIndexerErrors(10, 60)
	c.indexer.Start()

	c.started.Store(true)
}

func (c *ElasticSearchStorage) Start() {
	go c.start()
}

func (c *ElasticSearchStorage) Stop() {
	if c.started.Load() == true {
		c.indexer.Stop()
		c.connection.Close()
	}
}

func New() (*ElasticSearchStorage, error) {
	c := elastigo.NewConn()

	elasticonfig := strings.Split(config.GetConfig().GetString("storage.elasticsearch"), ":")
	if len(elasticonfig) != 2 {
		return nil, ErrBadConfig
	}
	c.Domain = elasticonfig[0]
	c.Port = elasticonfig[1]

	storage := &ElasticSearchStorage{connection: c}
	storage.started.Store(false)

	return storage, nil
}
