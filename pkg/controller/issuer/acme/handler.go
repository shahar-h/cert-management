/*
 * SPDX-FileCopyrightText: 2020 SAP SE or an SAP affiliate company and Gardener contributors
 *
 * SPDX-License-Identifier: Apache-2.0
 */

package acme

import (
	"fmt"

	corev1 "k8s.io/api/core/v1"

	"github.com/gardener/controller-manager-library/pkg/controllermanager/controller/reconcile"
	"github.com/gardener/controller-manager-library/pkg/logger"
	"github.com/gardener/controller-manager-library/pkg/resources"

	api "github.com/gardener/cert-management/pkg/apis/cert/v1alpha1"
	"github.com/gardener/cert-management/pkg/cert/legobridge"
	"github.com/gardener/cert-management/pkg/controller/issuer/core"
)

var acmeType = core.ACMEType

// NewACMEIssuerHandler creates an ACME IssuerHandler.
func NewACMEIssuerHandler(support *core.Support) (core.IssuerHandler, error) {
	return &acmeIssuerHandler{
		support: support,
	}, nil
}

type acmeIssuerHandler struct {
	support *core.Support
}

func (r *acmeIssuerHandler) Type() string {
	return core.ACMEType
}

func (r *acmeIssuerHandler) CanReconcile(issuer *api.Issuer) bool {
	return issuer != nil && issuer.Spec.ACME != nil
}

func (r *acmeIssuerHandler) Reconcile(logger logger.LogContext, obj resources.Object, issuer *api.Issuer) reconcile.Status {
	logger.Infof("reconciling")

	acme := issuer.Spec.ACME
	if acme == nil {
		return r.failedAcme(logger, obj, api.StateError, fmt.Errorf("missing ACME spec"))
	}

	if acme.Email == "" {
		return r.failedAcme(logger, obj, api.StateError, fmt.Errorf("missing email in ACME spec"))
	}
	if acme.Server == "" {
		return r.failedAcme(logger, obj, api.StateError, fmt.Errorf("missing server in ACME spec"))
	}

	r.support.AddIssuerDomains(obj.ClusterKey(), acme.Domains)

	r.support.RememberIssuerSecret(obj.ClusterKey(), acme.PrivateKeySecretRef, "")

	issuerKey := r.support.ToIssuerKey(obj.ClusterKey())
	var secret *corev1.Secret
	var secretHash string
	var err error
	if acme.PrivateKeySecretRef != nil {
		secret, err = r.support.ReadIssuerSecret(issuerKey, acme.PrivateKeySecretRef)
		if err != nil {
			if acme.AutoRegistration {
				logger.Info("spec.acme.privateKeySecretRef not existing, creating new account")
			} else {
				return r.failedAcmeRetry(logger, obj, api.StateError, fmt.Errorf("loading issuer secret failed: %w", err))
			}
		}
		secretHash = r.support.CalcSecretHash(secret)
		r.support.RememberIssuerSecret(obj.ClusterKey(), acme.PrivateKeySecretRef, secretHash)
		r.support.RememberAltIssuerSecret(obj.ClusterKey(), acme.PrivateKeySecretRef, secret, acme.Email)
	}
	if secret != nil {
		objKey := obj.ClusterKey()
		eabKeyID, eabHmacKey, err := r.support.LoadEABHmacKey(&objKey, issuerKey, acme)
		if err != nil {
			return r.failedAcmeRetry(logger, obj, api.StateError, fmt.Errorf("loading EAB secret failed: %w", err))
		}
		var raw []byte
		if core.IsSameExistingRegistration(issuer.Status.ACME, secretHash) {
			raw = issuer.Status.ACME.Raw
		} else {
			user, err := legobridge.NewRegistrationUserFromEmail(issuerKey, acme.Email, acme.Server, secret.Data, eabKeyID, eabHmacKey)
			if err != nil {
				return r.failedAcmeRetry(logger, obj, api.StateError, fmt.Errorf("creating registration user failed: %w", err))
			}
			raw, err = user.RawRegistration()
			if err != nil {
				return r.failedAcme(logger, obj, api.StateError, fmt.Errorf("registration marshalling failed: %w", err))
			}
		}
		user, err := legobridge.RegistrationUserFromSecretData(issuerKey, acme.Email, acme.Server, raw,
			secret.Data, eabKeyID, eabHmacKey)
		if err != nil {
			return r.failedAcmeRetry(logger, obj, api.StateError, fmt.Errorf("extracting registration user from secret failed: %w", err))
		}
		if user.GetEmail() != acme.Email {
			return r.failedAcme(logger, obj, api.StateError, fmt.Errorf("email of registration user from secret does not match %s != %s", user.GetEmail(), acme.Email))
		}
		wrapped, err := core.WrapRegistration(raw, secretHash)
		if err != nil {
			return r.failedAcme(logger, obj, api.StateError, fmt.Errorf("wrapped registration marshalling failed: %w", err))
		}
		return r.support.SucceededAndTriggerCertificates(logger, obj, &acmeType, wrapped)
	} else if acme.AutoRegistration {
		user, err := legobridge.NewRegistrationUserFromEmail(issuerKey, acme.Email, acme.Server, nil, "", "")
		if err != nil {
			return r.failedAcmeRetry(logger, obj, api.StateError, fmt.Errorf("creating registration user failed: %w", err))
		}

		secretRef, secret, err := r.support.WriteIssuerSecretFromRegistrationUser(issuerKey, issuer.UID, user, acme.PrivateKeySecretRef)
		if err != nil {
			return r.failedAcmeRetry(logger, obj, api.StateError, fmt.Errorf("writing issuer secret failed: %w", err))
		}
		acme.PrivateKeySecretRef = secretRef
		secretHash = r.support.CalcSecretHash(secret)
		r.support.RememberIssuerSecret(obj.ClusterKey(), acme.PrivateKeySecretRef, secretHash)
		r.support.RememberAltIssuerSecret(obj.ClusterKey(), acme.PrivateKeySecretRef, secret, acme.Email)

		regRaw, err := user.RawRegistration()
		if err != nil {
			return r.failedAcme(logger, obj, api.StateError, fmt.Errorf("registration marshalling failed: %w", err))
		}
		newObj, err := r.support.GetIssuerResources(issuerKey).Update(issuer)
		if err != nil {
			return r.failedAcmeRetry(logger, obj, api.StateError, fmt.Errorf("updating resource failed: %w", err))
		}

		raw, err := core.WrapRegistration(regRaw, secretHash)
		if err != nil {
			return r.failedAcme(logger, obj, api.StateError, fmt.Errorf("wrapped registration marshalling failed: %w", err))
		}
		return r.support.SucceededAndTriggerCertificates(logger, newObj, &acmeType, raw)
	} else {
		return r.failedAcme(logger, obj, api.StateError, fmt.Errorf("neither `SecretRef` or `AutoRegistration: true` provided"))
	}
}

func (r *acmeIssuerHandler) failedAcme(logger logger.LogContext, obj resources.Object, state string, err error) reconcile.Status {
	return r.support.Failed(logger, obj, state, &acmeType, err, false)
}

func (r *acmeIssuerHandler) failedAcmeRetry(logger logger.LogContext, obj resources.Object, state string, err error) reconcile.Status {
	return r.support.Failed(logger, obj, state, &acmeType, err, true)
}
