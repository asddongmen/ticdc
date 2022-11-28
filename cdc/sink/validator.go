// Copyright 2022 PingCAP, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// See the License for the specific language governing permissions and
// limitations under the License.

package sink

import (
	"context"
	"net/url"
	"strings"

	"github.com/pingcap/tiflow/cdc/contextutil"
	"github.com/pingcap/tiflow/cdc/model"
	"github.com/pingcap/tiflow/cdc/sinkv2/eventsink/factory"
	"github.com/pingcap/tiflow/pkg/config"
	cerror "github.com/pingcap/tiflow/pkg/errors"
	"github.com/pingcap/tiflow/pkg/sink"
	pmysql "github.com/pingcap/tiflow/pkg/sink/mysql"
	"github.com/pingcap/tiflow/pkg/util"
)

// Validate sink if given valid parameters.
// TODO: For now, we create a real sink instance and validate it.
// Maybe we should support the dry-run mode to validate sink.
func Validate(ctx context.Context, sinkURI string, cfg *config.ReplicaConfig) error {
	var err error
	if err = preCheckSinkURI(sinkURI); err != nil {
		return err
	}

	if cfg.BDRMode {
		err = checkBDRMode(ctx, sinkURI, cfg)
		if err != nil {
			return err
		}
	}

	errCh := make(chan error)
	ctx, cancel := context.WithCancel(contextutil.PutRoleInCtx(ctx, util.RoleClient))
	conf := config.GetGlobalServerConfig()
	if !conf.Debug.EnableNewSink {
		var s Sink
		s, err = New(ctx, model.DefaultChangeFeedID("sink-verify"), sinkURI, cfg, errCh)
		if err != nil {
			cancel()
			return err
		}
		// NOTICE: We have to cancel the context before we close it,
		// otherwise we will write data to closed chan after sink closed.
		cancel()
		err = s.Close(ctx)
	} else {
		var s *factory.SinkFactory
		s, err = factory.New(ctx, sinkURI, cfg, errCh)
		if err != nil {
			cancel()
			return err
		}
		cancel()
		err = s.Close()
	}
	if err != nil {
		return err
	}
	select {
	case err = <-errCh:
		if err != nil {
			return err
		}
	default:
	}
	return nil
}

// preCheckSinkURI do some pre-check for sink URI.
// 1. Check if sink URI is empty.
// 2. Check if we use correct IPv6 format in URI.(if needed)
func preCheckSinkURI(sinkURIStr string) error {
	if sinkURIStr == "" {
		return cerror.ErrSinkURIInvalid.GenWithStack("sink uri is empty")
	}

	sinkURI, err := url.Parse(sinkURIStr)
	if err != nil {
		return cerror.WrapError(cerror.ErrSinkURIInvalid, err)
	}

	// Check if we use the correct IPv6 address format.
	// Notice: We should not check the host name is empty or not,
	// because we have blackhole sink which has empty host name.
	// Also notice the host name different from host(host+port).
	if util.IsIPv6Address(sinkURI.Hostname()) &&
		!util.IsValidIPv6AddressFormatInURI(sinkURI.Host) {
		return cerror.ErrSinkURIInvalid.GenWithStack("sink uri host is not valid IPv6 address, " +
			"when using IPv6 address in URI, please use [ipv6-address]:port")
	}

	return nil
}

func checkBDRMode(ctx context.Context, sinkURI string, replicaConfig *config.ReplicaConfig) error {
	maskSinkUri, err := util.MaskSinkURI(sinkURI)
	if err != nil {
		return err
	}
	isDownstreamTiDBOrMySQL, err := checkIsDownstreamTiDBoRMySQL(sinkURI)
	if err != nil {
		return err
	}
	if !isDownstreamTiDBOrMySQL {
		return cerror.ErrSinkURIInvalid.
			GenWithStack("sink uri scheme is not supported in BDR mode, sink uri: %s", maskSinkUri)
	}
	cfg := pmysql.NewConfig()
	id := model.DefaultChangeFeedID("sink-verify")
	uri, err := url.Parse(sinkURI)
	if err != nil {
		return cerror.WrapError(cerror.ErrSinkURIInvalid, err)
	}
	err = cfg.Apply(ctx, id, uri, replicaConfig)
	if err != nil {
		return err
	}
	dsn, err := pmysql.GenBasicDSN(uri, cfg)
	if err != nil {
		return err
	}
	testDB, err := pmysql.GetTestDB(ctx, dsn, pmysql.CreateMySQLDBConn)
	if err != nil {
		return err
	}
	defer testDB.Close()
	supported, err := pmysql.CheckIfSupportBDRMode(ctx, testDB)
	if err != nil {
		return err
	}
	if !supported {
		// TODO: refine this error message
		return cerror.ErrSinkURIInvalid.
			GenWithStack("downstream database does not support BDR mode, "+
				"please check your config, sink uri: %s", maskSinkUri)
	}
	return nil
}

func checkIsDownstreamTiDBoRMySQL(sinkURI string) (bool, error) {
	var err error
	var uri *url.URL
	uri, err = url.Parse(sinkURI)
	if err != nil {
		return false, cerror.WrapError(cerror.ErrSinkURIInvalid, err)
	}
	schema := strings.ToLower(uri.Scheme)
	switch schema {
	case sink.MySQLScheme, sink.MySQLSSLScheme, sink.TiDBScheme, sink.TiDBSSLScheme:
		return true, nil
	}
	return false, nil
}
