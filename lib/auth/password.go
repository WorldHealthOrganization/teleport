// Copyright 2021 Gravitational, Inc
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package auth

import (
	"context"
	"crypto/subtle"
	"net/mail"

	"golang.org/x/crypto/bcrypt"

	"github.com/gravitational/trace"

	"github.com/gravitational/teleport"
	"github.com/gravitational/teleport/api/client/proto"
	"github.com/gravitational/teleport/api/constants"
	"github.com/gravitational/teleport/api/types"
	apievents "github.com/gravitational/teleport/api/types/events"
	"github.com/gravitational/teleport/lib/auth/u2f"
	"github.com/gravitational/teleport/lib/defaults"
	"github.com/gravitational/teleport/lib/events"
	"github.com/gravitational/teleport/lib/services"
	"github.com/gravitational/teleport/lib/utils"

	"github.com/pquerna/otp"
	"github.com/pquerna/otp/totp"
)

// This is bcrypt hash for password "barbaz".
var fakePasswordHash = []byte(`$2a$10$Yy.e6BmS2SrGbBDsyDLVkOANZmvjjMR890nUGSXFJHBXWzxe7T44m`)

// ChangePasswordWithTokenRequest defines a request to change user password
// DELETE IN 9.0.0 along with changePasswordWithToken http endpoint
// in favor of grpc ChangeUserAuthentication.
type ChangePasswordWithTokenRequest struct {
	// SecondFactorToken is the TOTP code.
	SecondFactorToken string `json:"second_factor_token"`
	// TokenID is the ID of a reset or invite token.
	TokenID string `json:"token"`
	// Password is user password string converted to bytes.
	Password []byte `json:"password"`
	// U2FRegisterResponse is U2F registration challenge response.
	U2FRegisterResponse *u2f.RegisterChallengeResponse `json:"u2f_register_response,omitempty"`
}

// ChangeUserAuthentication implements AuthService.ChangeUserAuthentication.
func (s *Server) ChangeUserAuthentication(ctx context.Context, req *proto.ChangeUserAuthenticationRequest) (*proto.ChangeUserAuthenticationResponse, error) {
	user, err := s.changeUserAuthentication(ctx, req)
	if err != nil {
		return nil, trace.Wrap(err)
	}

	// Check if a user can receive new recovery codes.
	_, emailErr := mail.ParseAddress(user.GetName())
	hasEmail := emailErr == nil
	hasMFA := req.GetNewMFARegisterResponse() != nil
	recoveryAllowed := s.isAccountRecoveryAllowed(ctx) == nil
	createRecoveryCodes := hasEmail && hasMFA && recoveryAllowed

	var recoveryCodes []string
	if createRecoveryCodes {
		recoveryCodes, err = s.generateAndUpsertRecoveryCodes(ctx, user.GetName())
		if err != nil {
			return nil, trace.Wrap(err)
		}
	}

	webSession, err := s.createUserWebSession(ctx, user)
	if err != nil {
		return nil, trace.Wrap(err)
	}

	sess, ok := webSession.(*types.WebSessionV2)
	if !ok {
		return nil, trace.BadParameter("unexpected WebSessionV2 type %T", sess)
	}

	return &proto.ChangeUserAuthenticationResponse{
		WebSession:    sess,
		RecoveryCodes: recoveryCodes,
	}, nil
}

// ResetPassword securely generates a new random password and assigns it to user.
// This method is used to invalidate existing user password during password
// reset process.
func (s *Server) ResetPassword(username string) (string, error) {
	user, err := s.GetUser(username, false)
	if err != nil {
		return "", trace.Wrap(err)
	}

	password, err := utils.CryptoRandomHex(defaults.ResetPasswordLength)
	if err != nil {
		return "", trace.Wrap(err)
	}

	err = s.UpsertPassword(user.GetName(), []byte(password))
	if err != nil {
		return "", trace.Wrap(err)
	}

	return password, nil
}

// ChangePassword updates users password based on the old password.
func (s *Server) ChangePassword(req services.ChangePasswordReq) error {
	ctx := context.TODO()
	// validate new password
	if err := services.VerifyPassword(req.NewPassword); err != nil {
		return trace.Wrap(err)

	}

	authPreference, err := s.GetAuthPreference(ctx)
	if err != nil {
		return trace.Wrap(err)
	}

	userID := req.User
	fn := func() error {
		secondFactor := authPreference.GetSecondFactor()
		switch secondFactor {
		case constants.SecondFactorOff:
			return s.checkPasswordWOToken(userID, req.OldPassword)
		case constants.SecondFactorOTP:
			_, err := s.checkPassword(userID, req.OldPassword, req.SecondFactorToken)
			return trace.Wrap(err)
		case constants.SecondFactorU2F:
			if req.U2FSignResponse == nil {
				return trace.AccessDenied("missing U2F sign response")
			}

			_, err := s.CheckU2FSignResponse(ctx, userID, req.U2FSignResponse)
			return trace.Wrap(err)
		case constants.SecondFactorOn:
			if req.SecondFactorToken != "" {
				_, err := s.checkPassword(userID, req.OldPassword, req.SecondFactorToken)
				return trace.Wrap(err)
			}
			if req.U2FSignResponse != nil {
				_, err := s.CheckU2FSignResponse(ctx, userID, req.U2FSignResponse)
				return trace.Wrap(err)
			}
			return trace.AccessDenied("missing second factor authentication")
		case constants.SecondFactorOptional:
			if req.SecondFactorToken != "" {
				_, err := s.checkPassword(userID, req.OldPassword, req.SecondFactorToken)
				return trace.Wrap(err)
			}
			if req.U2FSignResponse != nil {
				_, err := s.CheckU2FSignResponse(ctx, userID, req.U2FSignResponse)
				return trace.Wrap(err)
			}
			// Check that a user has no MFA devices registered.
			devs, err := s.Identity.GetMFADevices(ctx, userID, false)
			if err != nil && !trace.IsNotFound(err) {
				return trace.Wrap(err)
			}
			if len(devs) != 0 {
				// MFA devices registered but no MFA fields set in request.
				log.Warningf("MFA bypass attempt by user %q, access denied.", userID)
				return trace.AccessDenied("missing second factor authentication")
			}
			return nil
		}

		return trace.BadParameter("unsupported second factor method: %q", secondFactor)
	}

	if err := s.WithUserLock(userID, fn); err != nil {
		return trace.Wrap(err)
	}

	if err := s.UpsertPassword(userID, req.NewPassword); err != nil {
		return trace.Wrap(err)
	}

	if err := s.emitter.EmitAuditEvent(s.closeCtx, &apievents.UserPasswordChange{
		Metadata: apievents.Metadata{
			Type: events.UserPasswordChangeEvent,
			Code: events.UserPasswordChangeCode,
		},
		UserMetadata: apievents.UserMetadata{
			User: userID,
		},
	}); err != nil {
		log.WithError(err).Warn("Failed to emit password change event.")
	}
	return nil
}

// checkPasswordWOToken checks just password without checking OTP tokens
// used in case of SSH authentication, when token has been validated.
func (s *Server) checkPasswordWOToken(user string, password []byte) error {
	const errMsg = "invalid username or password"

	err := services.VerifyPassword(password)
	if err != nil {
		return trace.BadParameter(errMsg)
	}

	hash, err := s.GetPasswordHash(user)
	if err != nil && !trace.IsNotFound(err) {
		return trace.Wrap(err)
	}
	userFound := true
	if trace.IsNotFound(err) {
		userFound = false
		log.Debugf("Username %q not found, using fake hash to mitigate timing attacks.", user)
		hash = fakePasswordHash
	}

	if err = bcrypt.CompareHashAndPassword(hash, password); err != nil {
		log.Debugf("Password for %q does not match", user)
		return trace.BadParameter(errMsg)
	}

	// Careful! The bcrypt check above may succeed for an unknown user when the
	// provided password is "barbaz", which is what fakePasswordHash hashes to.
	if !userFound {
		return trace.BadParameter(errMsg)
	}

	return nil
}

type checkPasswordResult struct {
	mfaDev *types.MFADevice
}

// checkPassword checks the password and OTP token. Called by tsh or lib/web/*.
func (s *Server) checkPassword(user string, password []byte, otpToken string) (*checkPasswordResult, error) {
	err := s.checkPasswordWOToken(user, password)
	if err != nil {
		return nil, trace.Wrap(err)
	}

	mfaDev, err := s.checkOTP(user, otpToken)
	if err != nil {
		return nil, trace.Wrap(err)
	}
	return &checkPasswordResult{mfaDev: mfaDev}, nil
}

// checkOTP determines the type of OTP token used (for legacy HOTP support), fetches the
// appropriate type from the backend, and checks if the token is valid.
func (s *Server) checkOTP(user string, otpToken string) (*types.MFADevice, error) {
	var err error

	otpType, err := s.getOTPType(user)
	if err != nil {
		return nil, trace.Wrap(err)
	}

	switch otpType {
	case teleport.HOTP:
		otp, err := s.GetHOTP(user)
		if err != nil {
			return nil, trace.Wrap(err)
		}

		// look ahead n tokens to see if we can find a matching token
		if !otp.Scan(otpToken, defaults.HOTPFirstTokensRange) {
			return nil, trace.BadParameter("bad one time token")
		}

		// we need to upsert the hotp state again because the
		// counter was incremented
		if err := s.UpsertHOTP(user, otp); err != nil {
			return nil, trace.Wrap(err)
		}
	case teleport.TOTP:
		ctx := context.TODO()

		// get the previously used token to mitigate token replay attacks
		usedToken, err := s.GetUsedTOTPToken(user)
		if err != nil {
			return nil, trace.Wrap(err)
		}
		// we use a constant time compare function to mitigate timing attacks
		if subtle.ConstantTimeCompare([]byte(otpToken), []byte(usedToken)) == 1 {
			return nil, trace.BadParameter("previously used totp token")
		}

		devs, err := s.Identity.GetMFADevices(ctx, user, true)
		if err != nil {
			return nil, trace.Wrap(err)
		}

		for _, dev := range devs {
			totpDev := dev.GetTotp()
			if totpDev == nil {
				continue
			}

			if err := s.checkTOTP(ctx, user, otpToken, dev); err != nil {
				log.WithError(err).Errorf("Using TOTP device %q", dev.GetName())
				continue
			}
			return dev, nil
		}
		return nil, trace.AccessDenied("invalid totp token")
	}

	return nil, nil
}

// checkTOTP checks if the TOTP token is valid.
func (s *Server) checkTOTP(ctx context.Context, user, otpToken string, dev *types.MFADevice) error {
	if dev.GetTotp() == nil {
		return trace.BadParameter("checkTOTP called with non-TOTP MFADevice %T", dev.Device)
	}
	// we use totp.ValidateCustom over totp.Validate so we can use
	// a fake clock in tests to get reliable results
	valid, err := totp.ValidateCustom(otpToken, dev.GetTotp().Key, s.clock.Now(), totp.ValidateOpts{
		Period:    teleport.TOTPValidityPeriod,
		Skew:      teleport.TOTPSkew,
		Digits:    otp.DigitsSix,
		Algorithm: otp.AlgorithmSHA1,
	})
	if err != nil {
		return trace.AccessDenied("failed to validate TOTP code: %v", err)
	}
	if !valid {
		return trace.AccessDenied("TOTP code not valid")
	}
	// if we have a valid token, update the previously used token
	if err := s.UpsertUsedTOTPToken(user, otpToken); err != nil {
		return trace.Wrap(err)
	}

	// Update LastUsed timestamp on the device.
	dev.LastUsed = s.clock.Now()
	if err := s.UpsertMFADevice(ctx, user, dev); err != nil {
		return trace.Wrap(err)
	}
	return nil
}

// CreateSignupU2FRegisterRequest initiates registration for a new U2F token.
// The returned challenge should be sent to the client to sign.
func (s *Server) CreateSignupU2FRegisterRequest(tokenID string) (*u2f.RegisterChallenge, error) {
	ctx := context.TODO()
	cap, err := s.GetAuthPreference(ctx)
	if err != nil {
		return nil, trace.Wrap(err)
	}

	u2fConfig, err := cap.GetU2F()
	if err != nil {
		return nil, trace.Wrap(err)
	}

	_, err = s.getResetPasswordToken(context.TODO(), tokenID)
	if err != nil {
		return nil, trace.Wrap(err)
	}

	return u2f.RegisterInit(u2f.RegisterInitParams{
		StorageKey: tokenID,
		AppConfig:  *u2fConfig,
		Storage:    s.Identity,
	})
}

// getOTPType returns the type of OTP token used, HOTP or TOTP.
// Deprecated: Remove this method once HOTP support has been removed from Gravity.
func (s *Server) getOTPType(user string) (teleport.OTPType, error) {
	_, err := s.GetHOTP(user)
	if err != nil {
		if trace.IsNotFound(err) {
			return teleport.TOTP, nil
		}
		return "", trace.Wrap(err)
	}
	return teleport.HOTP, nil
}

func (s *Server) changeUserAuthentication(ctx context.Context, req *proto.ChangeUserAuthenticationRequest) (types.User, error) {
	// Get cluster configuration and check if local auth is allowed.
	authPref, err := s.GetAuthPreference(ctx)
	if err != nil {
		return nil, trace.Wrap(err)
	}
	if !authPref.GetAllowLocalAuth() {
		return nil, trace.AccessDenied(noLocalAuth)
	}

	err = services.VerifyPassword(req.GetNewPassword())
	if err != nil {
		return nil, trace.Wrap(err)
	}

	// Check if token exists.
	token, err := s.getResetPasswordToken(ctx, req.TokenID)
	if err != nil {
		return nil, trace.Wrap(err)
	}

	if token.Expiry().Before(s.clock.Now().UTC()) {
		return nil, trace.BadParameter("expired token")
	}

	err = s.changeUserSecondFactor(req, token)
	if err != nil {
		return nil, trace.Wrap(err)
	}

	username := token.GetUser()
	// Delete this token first to minimize the chances
	// of partially updated user with still valid token.
	err = s.deleteUserTokens(ctx, username)
	if err != nil {
		return nil, trace.Wrap(err)
	}

	// Set a new password.
	err = s.UpsertPassword(username, req.GetNewPassword())
	if err != nil {
		return nil, trace.Wrap(err)
	}

	user, err := s.GetUser(username, false)
	if err != nil {
		return nil, trace.Wrap(err)
	}

	return user, nil
}

func (s *Server) changeUserSecondFactor(req *proto.ChangeUserAuthenticationRequest, token types.UserToken) error {
	ctx := context.TODO()
	username := token.GetUser()
	cap, err := s.GetAuthPreference(ctx)
	if err != nil {
		return trace.Wrap(err)
	}

	secondFactor := cap.GetSecondFactor()
	if secondFactor == constants.SecondFactorOff {
		return nil
	}
	switch req.NewMFARegisterResponse.GetResponse().(type) {
	case *proto.MFARegisterResponse_TOTP:
		if secondFactor == constants.SecondFactorU2F {
			return trace.BadParameter("user %q sent an OTP token during password reset but cluster only allows U2F for second factor", username)
		}
		secrets, err := s.Identity.GetUserTokenSecrets(ctx, req.TokenID)
		if err != nil {
			return trace.Wrap(err)
		}

		dev, err := services.NewTOTPDevice("otp", secrets.GetOTPKey(), s.clock.Now())
		if err != nil {
			return trace.Wrap(err)
		}
		if err := s.checkTOTP(ctx, username, req.GetNewMFARegisterResponse().GetTOTP().GetCode(), dev); err != nil {
			return trace.Wrap(err)
		}
		if err := s.UpsertMFADevice(ctx, username, dev); err != nil {
			return trace.Wrap(err)
		}

		return nil

	case *proto.MFARegisterResponse_U2F:
		if secondFactor == constants.SecondFactorOTP {
			return trace.BadParameter("user %q sent a U2F registration during password reset but cluster only allows OTP for second factor", username)
		}
		cfg, err := cap.GetU2F()
		if err != nil {
			return trace.Wrap(err)
		}

		_, err = u2f.RegisterVerify(ctx, u2f.RegisterVerifyParams{
			DevName:                "u2f",
			ChallengeStorageKey:    req.TokenID,
			RegistrationStorageKey: username,
			Resp: u2f.RegisterChallengeResponse{
				RegistrationData: req.GetNewMFARegisterResponse().GetU2F().GetRegistrationData(),
				ClientData:       req.GetNewMFARegisterResponse().GetU2F().GetClientData(),
			},
			Storage:        s.Identity,
			Clock:          s.GetClock(),
			AttestationCAs: cfg.DeviceAttestationCAs,
		})
		return trace.Wrap(err)
	default:
		if secondFactor != constants.SecondFactorOptional {
			return trace.BadParameter("no second factor sent during user %q password reset", username)
		}
	}
	return nil
}
