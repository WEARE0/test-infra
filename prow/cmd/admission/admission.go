/*
Copyright 2018 The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package main

import (
	"encoding/json"
	"fmt"
	"io"
	"io/ioutil"
	"net/http"

	"github.com/sirupsen/logrus"

	admissionapi "k8s.io/api/admission/v1beta1"
	admissionregistrationv1beta1 "k8s.io/api/admissionregistration/v1beta1"
	"k8s.io/apimachinery/pkg/api/equality"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/serializer"
	prowjobv1 "k8s.io/test-infra/prow/apis/prowjobs/v1"

	prowjobscheme "k8s.io/test-infra/prow/client/clientset/versioned/scheme"
)

var (
	vscheme = runtime.NewScheme()
	codecs  = serializer.NewCodecFactory(vscheme)
)

func init() {
	if err := prowjobscheme.AddToScheme(vscheme); err != nil {
		logrus.Errorf("Add prow job scheme: %v", err)
	}
	if err := admissionapi.AddToScheme(vscheme); err != nil {
		logrus.Errorf("Add admission API scheme: %v", err)
	}
	if err := admissionregistrationv1beta1.AddToScheme(vscheme); err != nil {
		logrus.Errorf("Add admission registration scheme: %v", err)
	}
}

const contentTypeJSON = "application/json"

// readRequest extracts the request from the AdmissionReview reader
func readRequest(r io.Reader, contentType string) (*admissionapi.AdmissionRequest, error) {
	if contentType != contentTypeJSON {
		return nil, fmt.Errorf("Content-Type=%s, expected %s", contentType, contentTypeJSON)
	}

	// Can we read the body?
	if r == nil {
		return nil, fmt.Errorf("no body")
	}
	body, err := ioutil.ReadAll(r)
	if err != nil {
		return nil, fmt.Errorf("read body: %w", err)
	}

	// Can we convert the body into an AdmissionReview?
	var ar admissionapi.AdmissionReview
	deserializer := codecs.UniversalDeserializer()
	if _, _, err := deserializer.Decode(body, nil, &ar); err != nil {
		return nil, fmt.Errorf("decode body: %w", err)
	}
	return ar.Request, nil
}

// handle reads the request and writes the response
func handle(w http.ResponseWriter, r *http.Request) {
	req, err := readRequest(r.Body, r.Header.Get("Content-Type"))
	if err != nil {
		logrus.WithError(err).Error("read")
	}

	if err := writeResponse(*req, w, onlyUpdateStatus); err != nil {
		logrus.WithError(err).Error("write")
	}
}

type decider func(admissionapi.AdmissionRequest) (*admissionapi.AdmissionResponse, error)

// writeResponse gets the response from onlyUpdateStatus and writes it to w.
func writeResponse(ar admissionapi.AdmissionRequest, w io.Writer, decide decider) error {
	response, err := decide(ar)
	if err != nil {
		logrus.WithError(err).Error("failed decision")
		response = &admissionapi.AdmissionResponse{
			Result: &meta.Status{
				Message: err.Error(),
			},
		}
	}
	var result admissionapi.AdmissionReview
	result.Response = response
	result.Response.UID = ar.UID
	out, err := json.Marshal(result)
	if err != nil {
		return fmt.Errorf("encode response: %w", err)
	}
	if _, err := w.Write(out); err != nil {
		return fmt.Errorf("write response: %w", err)
	}
	return nil
}

var (
	allow = admissionapi.AdmissionResponse{
		Allowed: true,
	}
	reject = admissionapi.AdmissionResponse{
		Result: &meta.Status{
			Reason:  meta.StatusReasonForbidden,
			Message: "ProwJobs may only update status",
		},
	}
)

// onlyUpdateStatus returns the response to the request
func onlyUpdateStatus(req admissionapi.AdmissionRequest) (*admissionapi.AdmissionResponse, error) {
	logger := logrus.WithFields(logrus.Fields{
		"resource":    req.Resource,
		"subresource": req.SubResource,
		"name":        req.Name,
		"namespace":   req.Namespace,
		"operation":   req.Operation,
	})

	// Does this only update status?
	if req.SubResource == "status" {
		logrus.Info("accept status update")
		return &allow, nil
	}

	// Otherwise, do the specs match?
	var new prowjobv1.ProwJob
	if _, _, err := codecs.UniversalDeserializer().Decode(req.Object.Raw, nil, &new); err != nil {
		return nil, fmt.Errorf("decode new: %w", err)
	}
	var old prowjobv1.ProwJob
	if _, _, err := codecs.UniversalDeserializer().Decode(req.OldObject.Raw, nil, &old); err != nil {
		return nil, fmt.Errorf("decode old: %w", err)
	}
	if equality.Semantic.DeepEqual(old.Spec, new.Spec) {
		logrus.Info("accept update with equivalent spec")
		return &allow, nil // yes
	}
	logger.Info("reject") // no
	return &reject, nil
}
