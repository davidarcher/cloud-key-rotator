// Copyright 2019 OVO Technology
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

package rotate

import (
	"bytes"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	keys "github.com/ovotech/cloud-key-client"

	"github.com/ovotech/cloud-key-rotator/pkg/config"
	"github.com/ovotech/cloud-key-rotator/pkg/cred"
	"github.com/ovotech/cloud-key-rotator/pkg/location"
	"github.com/ovotech/cloud-key-rotator/pkg/log"
)

//rotationCandidate type
type rotationCandidate struct {
	key                   keys.Key
	keyLocation           config.KeyLocations
	rotationThresholdMins int
}

var logger = log.StdoutLogger().Sugar()

const (
	datadogURL = "https://api.datadoghq.com/api/v1/series?api_key="
)

//keyProviders returns a slice of key providers based on flags or config (in
// that order of priority)
func keyProviders(provider, project string, c config.Config) (keyProviders []keys.Provider) {
	if len(provider) > 0 {
		keyProviders = append(keyProviders, keys.Provider{GcpProject: project,
			Provider: provider})
	} else {
		for _, cloudProvider := range c.CloudProviders {
			keyProviders = append(keyProviders, keys.Provider{GcpProject: cloudProvider.Project,
				Provider: cloudProvider.Name})
		}
	}
	return
}

//validateFlags returns an error that's not nil if provided string values fail
// a set of validation rules
func validateFlags(account, provider, project string) (err error) {
	if len(account) > 0 && len(provider) == 0 {
		err = errors.New("Both account AND provider flags must be set")
		return
	}
	if provider == "gcp" && len(project) == 0 {
		err = errors.New("Project flag must be set when using the GCP provider")
		return
	}
	return
}

//keysOfProviders returns keys from all the configured providers that have passed
// through filtering
func keysOfProviders(account, provider, project string, c config.Config) (accountKeys []keys.Key, err error) {
	if accountKeys, err = keys.Keys(keyProviders(provider, project, c), c.IncludeInactiveKeys); err != nil {
		return
	}
	logger.Infof("Found %d keys in total", len(accountKeys))
	return filterKeys(accountKeys, c, account)
}

//Rotate rotates those keys..
func Rotate(account, provider, project string, c config.Config) (err error) {
	defer logger.Sync()

	if err = validateFlags(account, provider, project); err != nil {
		return
	}
	var providerKeys []keys.Key
	if providerKeys, err = keysOfProviders(account, provider, project, c); err != nil {
		return
	}
	logger.Infof("Filtered down to %d keys based on current app config", len(providerKeys))
	if !c.RotationMode {
		postMetric(providerKeys, c.DatadogAPIKey, c.Datadog)
		return
	}
	var rc []rotationCandidate
	if rc, err = rotationCandidates(account, providerKeys, c.AccountKeyLocations,
		c.Credentials, c.DefaultRotationAgeThresholdMins); err != nil {
		return
	}
	logger.Infof("Finalised %d keys that are candidates for rotation", len(rc))
	return rotateKeys(account, rc, c.Credentials)
}

//rotatekey creates a new key for the rotation candidate, updates its key locations,
// and deletes the old key iff the key location update is successful
func rotateKey(account string, rotationCandidate rotationCandidate, creds cred.Credentials) (err error) {
	key := rotationCandidate.key
	keyProvider := key.Provider.Provider
	var newKeyID string
	var newKey string
	if newKeyID, newKey, err = createKey(account, key, keyProvider); err != nil {
		return
	}
	keyWrapper := location.KeyWrapper{Key: newKey, KeyID: newKeyID, KeyProvider: keyProvider}
	if err = updateKeyLocation(account, rotationCandidate.keyLocation, keyWrapper, creds); err != nil {
		return
	}
	return deleteKey(account, key, keyProvider)
}

//rotationAgeThreshold calculates the key age rotation threshold based on config values
func rotationAgeThreshold(keyLocation config.KeyLocations, defaultRotationAgeThresholdMins int) (rotationAgeThresholdMins int) {
	rotationAgeThresholdMins = defaultRotationAgeThresholdMins
	if keyLocation.RotationAgeThresholdMins > 0 {
		rotationAgeThresholdMins = keyLocation.RotationAgeThresholdMins
	}
	return
}

//rotateKeys iterates over the rotation candidates, invoking the func that actually
// performs the rotation
func rotateKeys(account string, rotationCandidates []rotationCandidate, creds cred.Credentials) (err error) {
	for _, rc := range rotationCandidates {
		key := rc.key
		logger.Infow("Rotation process started",
			"keyProvider", key.Provider.Provider,
			"account", account,
			"keyID", key.ID,
			"keyAge", fmt.Sprintf("%f", key.Age),
			"keyAgeThreshold", strconv.Itoa(rc.rotationThresholdMins))

		if err = rotateKey(account, rc, creds); err != nil {
			return
		}
	}

	return
}

//rotatekeys runs through the end to end process of rotating a slice of keys:
//filter down to subset of target keys, generate new key for each, update the
//key's locations and finally delete the existing/old key
func rotationCandidates(account string, accountKeys []keys.Key, keyLoc []config.KeyLocations,
	creds cred.Credentials, defaultRotationAgeThresholdMins int) (rotationCandidates []rotationCandidate, err error) {
	processedItems := make([]string, 0)
	for _, key := range accountKeys {
		keyAccount := key.Account
		var locations config.KeyLocations

		if locations, err = accountKeyLocation(keyAccount, keyLoc); err != nil {
			return
		}

		if contains(processedItems, key.FullAccount) {
			logger.Infof("Skipping SA: %s, key: %s as a key for this account has already been added as a candidate for rotation",
				account, key.ID)
			continue
		}

		rotationThresholdMins := rotationAgeThreshold(locations, defaultRotationAgeThresholdMins)
		if float64(rotationThresholdMins) > key.Age {
			logger.Infof("Skipping SA: %s, key: %s as it's only %f minutes old (threshold: %d mins)",
				account, key.ID, key.Age, rotationThresholdMins)
			continue
		}

		rotationCandidates = append(rotationCandidates, rotationCandidate{key: key,
			keyLocation:           locations,
			rotationThresholdMins: rotationThresholdMins})
		processedItems = append(processedItems, key.FullAccount)
	}

	return
}

//createKey creates a new key with the provider specified
func createKey(account string, key keys.Key, keyProvider string) (newKeyID, newKey string, err error) {
	if newKeyID, newKey, err = keys.CreateKey(key); err != nil {
		logger.Error(err)
		return
	}
	logger.Infow("New key created",
		"keyProvider", keyProvider,
		"account", account,
		"keyID", newKeyID)
	return
}

//deletekey deletes the key
func deleteKey(account string, key keys.Key, keyProvider string) (err error) {
	if err = keys.DeleteKey(key); err != nil {
		return
	}
	logger.Infow("Old key deleted",
		"keyProvider", keyProvider,
		"account", account,
		"keyID", key.ID)
	return
}

//accountKeyLocation gets the keyLocation element defined in config for the
//specified account
func accountKeyLocation(account string,
	keyLocations []config.KeyLocations) (accountKeyLocation config.KeyLocations, err error) {
	err = errors.New("No account key locations (in config) mapped to SA: " + account)
	for _, keyLocation := range keyLocations {
		if account == keyLocation.ServiceAccountName {
			err = nil
			accountKeyLocation = keyLocation
			break
		}
	}
	return
}

//locationsToUpdate return a slice of structs that implement the keyWriter
// interface, based on the keyLocations supplied
func locationsToUpdate(keyLocation config.KeyLocations) (kws []location.KeyWriter) {

	// read locations
	for _, circleCI := range keyLocation.CircleCI {
		kws = append(kws, circleCI)
	}

	if len(keyLocation.GitHub.OrgRepo) > 0 {
		kws = append(kws, keyLocation.GitHub)
	}

	for _, k8s := range keyLocation.K8s {
		kws = append(kws, k8s)
	}

	return
}

//updateKeyLocation updates locations specified in keyLocations with the new key, e.g. GitHub, CircleCI an K8s
func updateKeyLocation(account string, keyLocations config.KeyLocations,
	keyWrapper location.KeyWrapper, creds cred.Credentials) (err error) {

	// update locations
	var updatedLocations []location.UpdatedLocation

	for _, locationToUpdate := range locationsToUpdate(keyLocations) {

		var updated location.UpdatedLocation

		if updated, err = locationToUpdate.Write(keyLocations.ServiceAccountName, keyWrapper, creds); err != nil {
			return
		}

		updatedLocations = append(updatedLocations, updated)
	}

	// all done
	logger.Infow("Key locations updated",
		"keyProvider", keyWrapper.KeyProvider,
		"account", account,
		"keyID", keyWrapper.KeyID,
		"keyLocationUpdates", updatedLocations)

	return
}

//validKey returns a bool reflecting whether the key is deemed to be valid, based
// on a number of provider-specific rules. E.g., if the provider is AWS, and
// not configured to include user keys, is the key a user key (and hence invalid)?
func validKey(key keys.Key, config config.Config) bool {
	if key.Provider.Provider == "aws" {
		return validAwsKey(key, config)
	}
	return true
}

//filterKeys returns a keys.Key slice created by filtering the provided
// keys.Key slice based on specific rules for each provider
func filterKeys(keysToFilter []keys.Key, config config.Config, account string) (filteredKeys []keys.Key, err error) {
	var selfKeys []keys.Key
	for _, key := range keysToFilter {
		//valid bool is used to filter out keys early, e.g. if config says don't
		//include AWS user keys, and the current key happens to be a user key
		if !validKey(key, config) {
			continue
		}
		var eligible bool
		if eligible, err = filterKey(account, config, key); err != nil {
			return
		}
		if eligible {
			//don't add the key to filteredKeys yet if it's deemed to be a 'self' key
			// (i.e. the key belongs to the process performing this rotation)
			if isSelf(config, key) {
				logger.Infow("Key has been identified as a cloud-rotator key, so will be processed last",
					"keyProvider", key.Provider,
					"account", key.Account)
				selfKeys = append(selfKeys, key)
			} else {
				filteredKeys = append(filteredKeys, key)
			}
		}
	}
	//now add the 'self' keys
	filteredKeys = append(filteredKeys, selfKeys...)
	return
}

//isSelf returns true iff the key provided matches the 'self' defined in the
// config.cloudProvider. This means the key is the one being used in the
// rotation process, and should probably be rotated last.
func isSelf(config config.Config, key keys.Key) bool {
	for _, cloudProvider := range config.CloudProviders {
		if cloudProvider.Name == key.Provider.Provider &&
			cloudProvider.Project == key.Provider.GcpProject &&
			cloudProvider.Self == key.Account {
			return true
		}
	}
	return false
}

//filterKey returns a bool indicating whether the key is eligible for 'use'
func filterKey(account string, config config.Config, key keys.Key) (eligible bool, err error) {
	if len(account) > 0 {
		//this means an overriding account has been supplied, i.e. from CLI
		eligible = key.Account == account
	} else if !config.RotationMode {
		//rotation mode is false, so include the key so its age can be used
		eligible = true
	} else {
		if eligible, err = isKeyEligible(config, key); err != nil {
			return
		}
	}
	return
}

//isKeyEligible returns a bool indicating whether the key is eligible based on
// application config
func isKeyEligible(config config.Config, key keys.Key) (eligible bool, err error) {
	filterAccounts := config.AccountFilter.Accounts
	filterMode := config.AccountFilter.Mode
	switch filterMode {
	case "include":
		eligible = keyDefinedInFiltering(filterAccounts, key)
	case "exclude":
		eligible = !keyDefinedInFiltering(filterAccounts, key)
	default:
		err = fmt.Errorf("Filter mode: %s is not supported", filterMode)
	}
	return
}

//keyDefinedInFiltering returns a bool indicating whether the key matches
// a service account defined in the AccountFilter
func keyDefinedInFiltering(providerServiceAccounts []config.ProviderServiceAccounts,
	key keys.Key) bool {
	for _, psa := range providerServiceAccounts {
		if psa.Provider.Name == key.Provider.Provider &&
			psa.Provider.Project == key.Provider.GcpProject {
			for _, sa := range psa.ProviderAccounts {
				if sa == key.Account {
					return true
				}
			}
		}
	}

	return false
}

//contains returns true if the string slice contains the specified string
func contains(s []string, e string) bool {
	for _, a := range s {
		if a == e {
			return true
		}
	}
	return false
}

//validAwsKey returns a bool that reflects whether the provided keys.Key is
// valid, based on aws-specific rules
func validAwsKey(key keys.Key, config config.Config) (valid bool) {
	if config.IncludeAwsUserKeys {
		valid = true
	} else {
		match, _ := regexp.MatchString("[a-zA-Z]\\.[a-zA-Z]", key.Name)
		valid = !match
	}
	return
}

//postMetric posts details of each keys.Key to a metrics api
func postMetric(keys []keys.Key, apiKey string, datadog config.Datadog) (err error) {
	if len(apiKey) > 0 {
		url := strings.Join([]string{datadogURL, apiKey}, "")
		for _, key := range keys {
			var jsonString = []byte(
				`{ "series" :[{"metric":"` + datadog.MetricName + `",` +
					`"points":[[` +
					strconv.FormatInt(time.Now().Unix(), 10) +
					`, ` + strconv.FormatFloat(key.Age, 'f', 2, 64) +
					`]],` +
					`"type":"count",` +
					`"tags":[` +
					`"team:` + datadog.MetricTeam + `",` +
					`"environment:` + datadog.MetricEnv + `",` +
					`"key:` + key.Name + `",` +
					`"provider:` + key.Provider.Provider + `",` +
					`"status:` + key.Status + `",` +
					`"account:` + key.Account +
					`"]}]}`)
			var req *http.Request
			if req, err = http.NewRequest("POST", url, bytes.NewBuffer(jsonString)); err != nil {
				return
			}
			req.Header.Set("Content-type", "application/json")
			client := &http.Client{}
			var resp *http.Response
			if resp, err = client.Do(req); err != nil {
				return
			}
			defer resp.Body.Close()
			if resp.StatusCode != 202 {
				err = fmt.Errorf("non-202 status code (%d) returned by Datadog", resp.StatusCode)
			}
		}
	}
	return
}