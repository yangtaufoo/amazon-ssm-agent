// Copyright 2016 Amazon.com, Inc. or its affiliates. All Rights Reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License"). You may not
// use this file except in compliance with the License. A copy of the
// License is located at
//
// http://aws.amazon.com/apache2.0/
//
// or in the "license" file accompanying this file. This file is distributed
// on an "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND,
// either express or implied. See the License for the specific language governing
// permissions and limitations under the License.

// Package service wraps SSM service
package service

import (
	"fmt"
	"time"

	"sync"

	"github.com/aws/amazon-ssm-agent/agent/appconfig"
	"github.com/aws/amazon-ssm-agent/agent/association/cache"
	"github.com/aws/amazon-ssm-agent/agent/association/model"
	"github.com/aws/amazon-ssm-agent/agent/contracts"
	"github.com/aws/amazon-ssm-agent/agent/jsonutil"
	"github.com/aws/amazon-ssm-agent/agent/log"
	"github.com/aws/amazon-ssm-agent/agent/sdkutil"
	ssmsvc "github.com/aws/amazon-ssm-agent/agent/ssm"
	"github.com/aws/amazon-ssm-agent/agent/times"
	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ssm"
	"github.com/twinj/uuid"
)

const (
	stopPolicyErrorThreshold       = 10
	latestDoc                      = "$LATEST"
	cronExpressionEveryFiveMinutes = "cron(0 0/5 * 1/1 * ? *)"
)

type associationApiMode string

const (
	instanceAssociationMode associationApiMode = "instanceAssociationMode"
	legacyAssociationMode   associationApiMode = "legacyAssociationMode"
)

var (
	currentAssociationApiMode associationApiMode = instanceAssociationMode
	lock                      sync.RWMutex
)

// T represents interface for association
type T interface {
	CreateNewServiceIfUnHealthy(log log.T)
	ListInstanceAssociations(log log.T, instanceID string) ([]*model.InstanceAssociation, error)
	LoadAssociationDetail(log log.T, assoc *model.InstanceAssociation) error
	UpdateAssociationStatus(
		log log.T,
		associationName string,
		instanceID string,
		status string,
		executionSummary string)
	UpdateInstanceAssociationStatus(
		log log.T,
		associationID string,
		associationName string,
		instanceID string,
		status string,
		errorCode string,
		executionDate string,
		executionSummary string)
	IsInstanceAssociationApiMode() bool
}

// AssociationService wraps the Ssm Service
type AssociationService struct {
	ssmSvc     ssmsvc.Service
	stopPolicy *sdkutil.StopPolicy
	name       string
}

// NewAssociationService returns a new association service
func NewAssociationService(name string) *AssociationService {
	ssmService := ssmsvc.NewService()
	policy := sdkutil.NewStopPolicy(name, stopPolicyErrorThreshold)
	svc := AssociationService{
		ssmSvc:     ssmService,
		stopPolicy: policy,
		name:       name,
	}

	return &svc
}

// CreateNewServiceIfUnHealthy checks service healthy and create new service if original is unhealthy
func (s *AssociationService) CreateNewServiceIfUnHealthy(log log.T) {
	if s.stopPolicy == nil {
		log.Debugf("creating new stop-policy.")
		s.stopPolicy = sdkutil.NewStopPolicy(s.name, stopPolicyErrorThreshold)
	}

	log.Debugf("assocProcessor's stoppolicy before polling is %v", s.stopPolicy)
	if !s.stopPolicy.IsHealthy() {
		log.Errorf("assocProcessor stopped temporarily due to internal failure. We will retry automatically")

		// reset stop policy and let the scheduler start the polling after pollMessageFrequencyMinutes timeout
		s.stopPolicy.ResetErrorCount()
		s.ssmSvc = ssmsvc.NewService()
		return
	}
}

// ListInstanceAssociations will get the Association and related document string
func (s *AssociationService) ListInstanceAssociations(log log.T, instanceID string) ([]*model.InstanceAssociation, error) {

	uuid.SwitchFormat(uuid.CleanHyphen)
	results := []*model.InstanceAssociation{}
	var parameterResponse *ssm.DescribeAssociationOutput

	response, err := s.ssmSvc.ListInstanceAssociations(log, instanceID, nil)
	// if ListInstanceAssociations return error, system will try to use legacy ListAssociations
	if err != nil {
		s.setAssociationApiMode(legacyAssociationMode)
		if results, err = s.ListAssociations(log, instanceID); err != nil {
			return results, fmt.Errorf("unable to retrieve associations %v", err)
		}
	} else {
		s.setAssociationApiMode(instanceAssociationMode)
		for {
			for _, assoc := range response.Associations {
				rawData := &model.InstanceAssociation{}
				rawData.Association = assoc
				rawData.CreateDate = time.Now().UTC()
				if rawData.Association.LastExecutionDate == nil {
					rawData.Association.LastExecutionDate = aws.Time(time.Time{}.UTC())
				}
				// legacy association do not have ScheduleExpression
				if rawData.Association.ScheduleExpression == nil || *rawData.Association.ScheduleExpression == "" {
					// Call descriptionAssociation to get the status
					if parameterResponse, err = s.ssmSvc.DescribeAssociation(log, instanceID, *assoc.Name); err != nil {
						log.Errorf("unable to retrieve association parameter, %v", err)
					}

					// If legacy association has already been executed then skip it
					if parameterResponse.AssociationDescription != nil &&
						parameterResponse.AssociationDescription.Status != nil &&
						*parameterResponse.AssociationDescription.Status.Name != contracts.AssociationStatusAssociated {
						log.Debugf("Skipping association %v as it has been processed", *assoc.Name)
						continue
					}

					rawData.LegacyAssociation = true
					rawData.Association.ScheduleExpression = aws.String(cronExpressionEveryFiveMinutes)
				}

				results = append(results, rawData)
			}

			if response.NextToken == nil || *response.NextToken == "" {
				break
			}

			if response, err = s.ssmSvc.ListInstanceAssociations(log, instanceID, response.NextToken); err != nil {
				return results, fmt.Errorf("unable to retrieve associations %v", err)
			}
		}
	}

	log.Debug("Number of associations is ", len(results))
	return results, nil
}

func (s *AssociationService) setAssociationApiMode(api associationApiMode) {
	lock.Lock()
	defer lock.Unlock()

	currentAssociationApiMode = api
}

// ListAssociations will get the Association and related document string from legacy api
func (s *AssociationService) ListAssociations(log log.T, instanceID string) ([]*model.InstanceAssociation, error) {

	var parameterResponse *ssm.DescribeAssociationOutput
	results := []*model.InstanceAssociation{}

	response, err := s.ssmSvc.ListAssociations(log, instanceID)
	if err != nil {
		return nil, fmt.Errorf("unable to retrieve associations %v", err)
	}

	if len(response.Associations) > 0 {
		// Legacy ListAssociation only supports one association to ba associated at a time
		assoc := response.Associations[0]

		// Call descriptionAssociation and retrieve the parameter json string
		if parameterResponse, err = s.ssmSvc.DescribeAssociation(log, instanceID, *assoc.Name); err != nil {
			log.Errorf("unable to retrieve association parameter, %v", err)
			return nil, err
		}

		// If legacy association has already been executed return empty
		if parameterResponse.AssociationDescription != nil &&
			parameterResponse.AssociationDescription.Status != nil &&
			*parameterResponse.AssociationDescription.Status.Name != contracts.AssociationStatusAssociated {
			log.Debugf("Skipping association %v as it has been processed", *assoc.Name)

			return results, nil
		}

		rawData := &model.InstanceAssociation{}
		rawData.Association = &ssm.InstanceAssociationSummary{
			AssociationId:      assoc.Name,
			DocumentVersion:    aws.String(latestDoc),
			Name:               assoc.Name,
			InstanceId:         aws.String(instanceID),
			Checksum:           aws.String(""),
			Parameters:         parameterResponse.AssociationDescription.Parameters,
			ScheduleExpression: aws.String(cronExpressionEveryFiveMinutes),
		}

		if rawData.Association.LastExecutionDate == nil {
			rawData.Association.LastExecutionDate = aws.Time(time.Time{}.UTC())
		}

		rawData.LegacyAssociation = true
		rawData.CreateDate = time.Now().UTC()
		results = append(results, rawData)
	}

	return results, nil
}

// UpdateInstanceAssociationStatus will get the Association and related document string
func (s *AssociationService) UpdateInstanceAssociationStatus(
	log log.T,
	associationID string,
	associationName string,
	instanceID string,
	status string,
	errorCode string,
	executionDate string,
	executionSummary string) {

	if s.IsInstanceAssociationApiMode() {
		date := times.ParseIso8601UTC(executionDate)
		executionResult := ssm.InstanceAssociationExecutionResult{
			Status:           aws.String(status),
			ErrorCode:        aws.String(errorCode),
			ExecutionDate:    aws.Time(date),
			ExecutionSummary: aws.String(executionSummary),
		}

		executionResultContent, err := jsonutil.Marshal(executionResult)
		if err != nil {
			log.Errorf("could not marshal associationStatus, %v", err)
			return
		}
		log.Info("Updating association status ", jsonutil.Indent(executionResultContent))

		response, err := s.ssmSvc.UpdateInstanceAssociationStatus(log, associationID, instanceID, &executionResult)
		if err != nil {
			log.Errorf("unable to update association status, %v", err)
			return
		}
		responseContent, err := jsonutil.Marshal(response)
		if err != nil {
			log.Error("could not marshal reponse! ", err)
			return
		}
		log.Debug("Update instance association status response content is ", jsonutil.Indent(responseContent))

		return
	}

	s.UpdateAssociationStatus(log, associationName, instanceID, status, executionSummary)

	return
}

// UsingInstanceAssociationApi represents if the agent is using new InstanceAssociationApi for listing and updating
func (s *AssociationService) IsInstanceAssociationApiMode() bool {
	lock.Lock()
	defer lock.Unlock()

	return currentAssociationApiMode == instanceAssociationMode
}

// LoadAssociationDetail loads document contents and parameters for the given association
func (s *AssociationService) LoadAssociationDetail(log log.T, assoc *model.InstanceAssociation) error {
	associationCache := cache.GetCache()
	associationID := assoc.Association.AssociationId

	// check if the association details have been cached
	if associationCache.IsCached(*associationID) {
		rawData := associationCache.Get(*associationID)
		assoc.Document = rawData.Document
		return nil
	}

	// if not cached before
	var (
		documentResponse *ssm.GetDocumentOutput
		err              error
	)

	// Call getDocument and retrieve the document json string
	if documentResponse, err = s.ssmSvc.GetDocument(log, *assoc.Association.Name, *assoc.Association.DocumentVersion); err != nil {
		log.Errorf("unable to retrieve document, %v", err)
		return err
	}

	assoc.Document = documentResponse.Content

	if err = associationCache.Add(*associationID, assoc); err != nil {
		return err
	}

	return nil
}

// UpdateAssociationStatus update association status
func (s *AssociationService) UpdateAssociationStatus(
	log log.T,
	associationName string,
	instanceID string,
	status string,
	executionSummary string) {

	config, err := appconfig.Config(false)
	if err != nil {
		log.Errorf("unable to load config, %v", err)
		return
	}

	agentInfoContent, err := jsonutil.Marshal(config.Agent)
	if err != nil {
		log.Error("could not marshal agentInfo! ", err)
		return
	}

	currentTime := time.Now().UTC()
	associationStatus := ssm.AssociationStatus{
		Name:           aws.String(status),
		Message:        aws.String(executionSummary),
		Date:           &currentTime,
		AdditionalInfo: &agentInfoContent,
	}

	associationStatusContent, err := jsonutil.Marshal(associationStatus)
	if err != nil {
		log.Error("could not marshal associationStatus! ", err)
		return
	}
	log.Debug("Update association status content is ", jsonutil.Indent(associationStatusContent))

	// Call getDocument and retrieve the document json string
	response, err := s.ssmSvc.UpdateAssociationStatus(
		log,
		instanceID,
		associationName,
		&associationStatus)

	if err != nil {
		log.Errorf("unable to update association status, %v", err)
		sdkutil.HandleAwsError(log, err, s.stopPolicy)
		return
	}

	responseContent, err := jsonutil.Marshal(response)
	if err != nil {
		log.Error("could not marshal reponse! ", err)
		return
	}
	log.Debug("Update association status response content is ", jsonutil.Indent(responseContent))
}
