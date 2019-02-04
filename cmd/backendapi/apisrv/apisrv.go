/*
package apisrv provides an implementation of the gRPC server defined in
../../../api/protobuf-spec/backend.proto

Copyright 2018 Google LLC

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    https://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.

*/

package apisrv

import (
	"context"
	"errors"
	"fmt"
	"net"
	"time"

	"github.com/GoogleCloudPlatform/open-match/internal/expbo"
	"github.com/GoogleCloudPlatform/open-match/internal/metrics"
	backend "github.com/GoogleCloudPlatform/open-match/internal/pb"
	redisHelpers "github.com/GoogleCloudPlatform/open-match/internal/statestorage/redis"
	"github.com/GoogleCloudPlatform/open-match/internal/statestorage/redis/ignorelist"
	"github.com/GoogleCloudPlatform/open-match/internal/statestorage/redis/redispb"
	"github.com/cenkalti/backoff"
	"github.com/gogo/protobuf/jsonpb"
	"github.com/gogo/protobuf/proto"
	log "github.com/sirupsen/logrus"
	"go.opencensus.io/plugin/ocgrpc"
	"go.opencensus.io/stats"
	"go.opencensus.io/tag"

	"github.com/tidwall/gjson"

	"github.com/gomodule/redigo/redis"
	"github.com/rs/xid"
	"github.com/spf13/viper"

	"google.golang.org/grpc"
)

// Logrus structured logging setup
var (
	beLogFields = log.Fields{
		"app":       "openmatch",
		"component": "backend",
	}
	beLog = log.WithFields(beLogFields)
)

// BackendAPI implements backend API Server, the server generated by compiling
// the protobuf, by fulfilling the API Client interface.
type BackendAPI struct {
	grpc *grpc.Server
	cfg  *viper.Viper
	pool *redis.Pool
}
type backendAPI BackendAPI

// New returns an instantiated srvice
func New(cfg *viper.Viper, pool *redis.Pool) *BackendAPI {
	s := BackendAPI{
		pool: pool,
		grpc: grpc.NewServer(grpc.StatsHandler(&ocgrpc.ServerHandler{})),
		cfg:  cfg,
	}

	// Add a hook to the logger to auto-count log lines for metrics output thru OpenCensus
	log.AddHook(metrics.NewHook(BeLogLines, KeySeverity))

	backend.RegisterBackendServer(s.grpc, (*backendAPI)(&s))
	beLog.Info("Successfully registered gRPC server")
	return &s
}

// Open starts the api grpc service listening on the configured port.
func (s *BackendAPI) Open() error {
	ln, err := net.Listen("tcp", ":"+s.cfg.GetString("api.backend.port"))
	if err != nil {
		beLog.WithFields(log.Fields{
			"error": err.Error(),
			"port":  s.cfg.GetInt("api.backend.port"),
		}).Error("net.Listen() error")
		return err
	}

	beLog.WithFields(log.Fields{"port": s.cfg.GetInt("api.backend.port")}).Info("TCP net listener initialized")

	go func() {
		err := s.grpc.Serve(ln)
		if err != nil {
			beLog.WithFields(log.Fields{"error": err.Error()}).Error("gRPC serve() error")
		}
		beLog.Info("serving gRPC endpoints")
	}()

	return nil
}

// CreateMatch is this service's implementation of the CreateMatch gRPC method
// defined in api/protobuf-spec/backend.proto
func (s *backendAPI) CreateMatch(c context.Context, profile *backend.MatchObject) (*backend.MatchObject, error) {

	// Get a cancel-able context
	ctx, cancel := context.WithCancel(c)
	defer cancel()

	// Create context for tagging OpenCensus metrics.
	funcName := "CreateMatch"
	fnCtx, _ := tag.New(ctx, tag.Insert(KeyMethod, funcName))

	// Generate a request to fill the profile. Make a unique request ID.
	moID := xid.New().String()
	requestKey := moID + "." + profile.Id

	/*
		// Debugging logs
		beLog.Info("Pools nil? ", (profile.Pools == nil))
		beLog.Info("Pools empty? ", (len(profile.Pools) == 0))
		beLog.Info("Rosters nil? ", (profile.Rosters == nil))
		beLog.Info("Rosters empty? ", (len(profile.Rosters) == 0))
		beLog.Info("config set for json.pools?", s.cfg.IsSet("jsonkeys.pools"))
		beLog.Info("contents key?", s.cfg.GetString("jsonkeys.pools"))
		beLog.Info("contents exist?", gjson.Get(profile.Properties, s.cfg.GetString("jsonkeys.pools")).Exists())
	*/

	// Case where no protobuf pools was passed; check if there's a JSON version in the properties.
	// This is for backwards compatibility, it is recommended you populate the protobuf's
	// 'pools' field directly and pass it to CreateMatch/ListMatches
	if profile.Pools == nil && s.cfg.IsSet("jsonkeys.pools") &&
		gjson.Get(profile.Properties, s.cfg.GetString("jsonkeys.pools")).Exists() {
		poolsJSON := fmt.Sprintf("{\"pools\": %v}", gjson.Get(profile.Properties, s.cfg.GetString("jsonkeys.pools")).String())
		ppLog := beLog.WithFields(log.Fields{"jsonkey": s.cfg.GetString("jsonkeys.pools")})
		ppLog.Info("poolsJSON: ", poolsJSON)

		ppools := &backend.MatchObject{}
		err := jsonpb.UnmarshalString(poolsJSON, ppools)
		if err != nil {
			ppLog.Error("failed to parse JSON to protobuf pools")
		} else {
			profile.Pools = ppools.Pools
			ppLog.Info("parsed JSON to protobuf pools")
		}
	}

	// Case where no protobuf roster was passed; check if there's a JSON version in the properties.
	// This is for backwards compatibility, it is recommended you populate the
	// protobuf's 'rosters' field directly and pass it to CreateMatch/ListMatches
	if profile.Rosters == nil && s.cfg.IsSet("jsonkeys.rosters") &&
		gjson.Get(profile.Properties, s.cfg.GetString("jsonkeys.rosters")).Exists() {
		rostersJSON := fmt.Sprintf("{\"rosters\": %v}", gjson.Get(profile.Properties, s.cfg.GetString("jsonkeys.rosters")).String())
		rLog := beLog.WithFields(log.Fields{"jsonkey": s.cfg.GetString("jsonkeys.rosters")})

		prosters := &backend.MatchObject{}
		err := jsonpb.UnmarshalString(rostersJSON, prosters)
		if err != nil {
			rLog.Error("failed to parse JSON to protobuf rosters")
		} else {
			profile.Rosters = prosters.Rosters
			rLog.Info("parsed JSON to protobuf rosters")
		}
	}

	// Add fields for all subsequent logging
	beLog = beLog.WithFields(log.Fields{
		"profileID":     profile.Id,
		"func":          funcName,
		"matchObjectID": moID,
		"requestKey":    requestKey,
	})
	beLog.Info("gRPC call executing")
	beLog.Info("profile is")
	beLog.Info(profile)

	// Write profile to state storage
	err := redispb.MarshalToRedis(ctx, s.pool, profile, s.cfg.GetInt("redis.expirations.matchobject"))
	if err != nil {
		beLog.WithFields(log.Fields{
			"error":     err.Error(),
			"component": "statestorage",
		}).Error("State storage failure to create match profile")

		// Failure! Return empty match object and the error
		stats.Record(fnCtx, BeGrpcErrors.M(1))
		return &backend.MatchObject{}, err
	}
	beLog.Info("Profile written to state storage")

	// Queue the request ID to be sent to an MMF
	_, err = redisHelpers.Update(ctx, s.pool, s.cfg.GetString("queues.profiles.name"), requestKey)
	if err != nil {
		beLog.WithFields(log.Fields{
			"error":     err.Error(),
			"component": "statestorage",
		}).Error("State storage failure to queue profile")

		// Failure! Return empty match object and the error
		stats.Record(fnCtx, BeGrpcErrors.M(1))
		return &backend.MatchObject{}, err
	}
	beLog.Info("Profile added to processing queue")

	watcherBO := backoff.NewExponentialBackOff()
	if err := expbo.UnmarshalExponentialBackOff(s.cfg.GetString("api.backend.backoff"), watcherBO); err != nil {
		beLog.WithError(err).Warn("Could not parse backoff string, using default backoff parameters for MatchObject watcher")
	}

	watcherBOCtx := backoff.WithContext(watcherBO, ctx)

	// get and return matchobject, it will be written to the requestKey when the MMF has finished.
	watchChan := redispb.Watcher(watcherBOCtx, s.pool, backend.MatchObject{Id: requestKey}) // Watcher() runs the appropriate Redis commands.
	newMO, ok := <-watchChan
	if !ok {
		// ok is false if watchChan has been closed by redispb.Watcher()
		// This happens when Watcher stops because of context cancellation or backing off reached time limit
		stats.Record(fnCtx, BeGrpcRequests.M(1))
		if watcherBOCtx.Context().Err() != nil {
			newMO.Error = "channel closed: " + watcherBOCtx.Context().Err().Error()
		} else {
			newMO.Error = "channel closed: backoff deadline exceeded"
		}
		return &newMO, errors.New("Error retrieving matchmaking results from state storage: " + newMO.Error)
	}

	// 'ok' was true, so properties should contain the results from redis.
	// Do basic error checking on the returned JSON
	if !gjson.Valid(profile.Properties) {
		newMO.Error = "retreived properties json was malformed"
	}

	// TODO test that this is the correct condition for an empty error.
	if newMO.Error != "" {
		stats.Record(fnCtx, BeGrpcErrors.M(1))
		return &newMO, errors.New(newMO.Error)
	}

	beLog.Info("Matchmaking results received, returning to backend client")
	stats.Record(fnCtx, BeGrpcRequests.M(1))
	return &newMO, err
}

// ListMatches is this service's implementation of the ListMatches gRPC method
// defined in api/protobuf-spec/backend.proto
// This is the streaming version of CreateMatch - continually submitting the
// profile to be filled until the requesting service ends the connection.
func (s *backendAPI) ListMatches(p *backend.MatchObject, matchStream backend.Backend_ListMatchesServer) error {

	// call creatematch in infinite loop as long as the stream is open
	ctx := matchStream.Context() // https://talks.golang.org/2015/gotham-grpc.slide#30

	// Create context for tagging OpenCensus metrics.
	funcName := "ListMatches"
	fnCtx, _ := tag.New(ctx, tag.Insert(KeyMethod, funcName))

	beLog = beLog.WithFields(log.Fields{"func": funcName})
	beLog.WithFields(log.Fields{
		"profileID": p.Id,
	}).Info("gRPC call executing. Calling CreateMatch. Looping until cancelled.")

	for {
		select {
		case <-ctx.Done():
			// Context cancelled, probably because the client cancelled their request, time to exit.
			beLog.WithFields(log.Fields{
				"profileID": p.Id,
			}).Info("gRPC Context cancelled; client is probably finished receiving matches")

			// TODO: need to make sure that in-flight matches don't get leaked here.
			stats.Record(fnCtx, BeGrpcRequests.M(1))
			return nil

		default:
			// Retreive results from Redis
			requestProfile := proto.Clone(p).(*backend.MatchObject)
			/*
				beLog.Debug("new profile requested!")
				beLog.Debug(requestProfile)
				beLog.Debug(&requestProfile)
			*/
			mo, err := s.CreateMatch(ctx, requestProfile)

			beLog = beLog.WithFields(log.Fields{"func": funcName})

			if err != nil {
				beLog.WithFields(log.Fields{"error": err.Error()}).Error("Failure calling CreateMatch")
				stats.Record(fnCtx, BeGrpcErrors.M(1))
				return err
			}
			beLog.WithFields(log.Fields{"matchProperties": fmt.Sprintf("%v", mo)}).Debug("Streaming back match object")
			matchStream.Send(mo)

			// TODO: This should be tunable, but there should be SOME sleep here, to give a requestor a window
			// to cleanly close the connection after receiving a match object when they know they don't want to
			// request any more matches.
			time.Sleep(2 * time.Second)
		}
	}
}

// DeleteMatch is this service's implementation of the DeleteMatch gRPC method
// defined in api/protobuf-spec/backend.proto
func (s *backendAPI) DeleteMatch(ctx context.Context, mo *backend.MatchObject) (*backend.Result, error) {

	// Create context for tagging OpenCensus metrics.
	funcName := "DeleteMatch"
	fnCtx, _ := tag.New(ctx, tag.Insert(KeyMethod, funcName))

	beLog = beLog.WithFields(log.Fields{"func": funcName})
	beLog.WithFields(log.Fields{
		"matchObjectID": mo.Id,
	}).Info("gRPC call executing")

	err := redisHelpers.Delete(ctx, s.pool, mo.Id)
	if err != nil {
		beLog.WithFields(log.Fields{
			"error":     err.Error(),
			"component": "statestorage",
		}).Error("State storage error")

		stats.Record(fnCtx, BeGrpcErrors.M(1))
		return &backend.Result{Success: false, Error: err.Error()}, err
	}

	beLog.WithFields(log.Fields{
		"matchObjectID": mo.Id,
	}).Info("Match Object deleted.")

	stats.Record(fnCtx, BeGrpcRequests.M(1))
	return &backend.Result{Success: true, Error: ""}, err
}

// CreateAssignments is this service's implementation of the CreateAssignments gRPC method
// defined in api/protobuf-spec/backend.proto
func (s *backendAPI) CreateAssignments(ctx context.Context, a *backend.Assignments) (*backend.Result, error) {

	// Make a map of players and what assignments we want to send them.
	playerIDs := make([]string, 0)
	players := make(map[string]string, 0)
	for _, roster := range a.Rosters { // Loop through all rosters
		for _, player := range roster.Players { // Loop through all players in this roster
			if player.Id != "" {
				if player.Assignment == "" {
					// No player-specific assignment, so use the default one in
					// the Assignment message.
					player.Assignment = a.Assignment
				}
				players[player.Id] = player.Assignment
				beLog.Debug(fmt.Sprintf("playerid %v assignment %v", player.Id, player.Assignment))
			}
		}
		playerIDs = append(playerIDs, getPlayerIdsFromRoster(roster)...)
	}

	// Create context for tagging OpenCensus metrics.
	funcName := "CreateAssignments"
	fnCtx, _ := tag.New(ctx, tag.Insert(KeyMethod, funcName))

	beLog = beLog.WithFields(log.Fields{"func": funcName})
	beLog.WithFields(log.Fields{
		"numAssignments": len(players),
	}).Info("gRPC call executing")

	// TODO: These two calls are done in two different transactions; could be
	// combined as an optimization but probably not particularly necessary
	// Send the players their assignments.
	err := redisHelpers.UpdateMultiFields(ctx, s.pool, players, "assignment")

	// Move these players from the proposed list to the deindexed list.
	ignorelist.Move(ctx, s.pool, playerIDs, "proposed", "deindexed")

	// Issue encountered
	if err != nil {
		beLog.WithFields(log.Fields{
			"error":     err.Error(),
			"component": "statestorage",
		}).Error("State storage error")

		stats.Record(fnCtx, BeGrpcErrors.M(1))
		stats.Record(fnCtx, BeAssignmentFailures.M(int64(len(players))))
		return &backend.Result{Success: false, Error: err.Error()}, err
	}

	// Success!
	beLog.WithFields(log.Fields{
		"numPlayers": len(players),
	}).Info("Assignments complete")

	stats.Record(fnCtx, BeGrpcRequests.M(1))
	stats.Record(fnCtx, BeAssignments.M(int64(len(players))))
	return &backend.Result{Success: true, Error: ""}, err
}

// DeleteAssignments is this service's implementation of the DeleteAssignments gRPC method
// defined in api/protobuf-spec/backend.proto
func (s *backendAPI) DeleteAssignments(ctx context.Context, r *backend.Roster) (*backend.Result, error) {
	assignments := getPlayerIdsFromRoster(r)

	// Create context for tagging OpenCensus metrics.
	funcName := "DeleteAssignments"
	fnCtx, _ := tag.New(ctx, tag.Insert(KeyMethod, funcName))

	beLog = beLog.WithFields(log.Fields{"func": funcName})
	beLog.WithFields(log.Fields{
		"numAssignments": len(assignments),
	}).Info("gRPC call executing")

	err := redisHelpers.DeleteMultiFields(ctx, s.pool, assignments, "assignment")

	// Issue encountered
	if err != nil {
		beLog.WithFields(log.Fields{
			"error":     err.Error(),
			"component": "statestorage",
		}).Error("State storage error")

		stats.Record(fnCtx, BeGrpcErrors.M(1))
		stats.Record(fnCtx, BeAssignmentDeletionFailures.M(int64(len(assignments))))
		return &backend.Result{Success: false, Error: err.Error()}, err
	}

	// Success!
	stats.Record(fnCtx, BeGrpcRequests.M(1))
	stats.Record(fnCtx, BeAssignmentDeletions.M(int64(len(assignments))))
	return &backend.Result{Success: true, Error: ""}, err
}

// getPlayerIdsFromRoster returns the slice of player ID strings contained in
// the input roster.
func getPlayerIdsFromRoster(r *backend.Roster) []string {
	playerIDs := make([]string, 0)
	for _, p := range r.Players {
		playerIDs = append(playerIDs, p.Id)
	}
	return playerIDs

}
