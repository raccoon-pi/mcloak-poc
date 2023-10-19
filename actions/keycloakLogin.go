package actions

import (
	b64 "encoding/base64"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/sts"
	"github.com/pkg/errors"
	"github.com/sirupsen/logrus"
	"github.com/versent/saml2aws/v2"
	"github.com/versent/saml2aws/v2/pkg/awsconfig"
	"github.com/versent/saml2aws/v2/pkg/cfg"
	"github.com/versent/saml2aws/v2/pkg/creds"
)

func Login(account *cfg.IDPAccount, loginDetails *creds.LoginDetails) (*awsconfig.AWSCredentials, error) {

	logger := logrus.WithField("command", "login")

	logger.Debug("Check if creds exist.")

	// loginDetails, err := resolveLoginDetails(account, loginFlags)
	// if err != nil {
	// 	log.Printf("%+v", err)
	// 	os.Exit(1)
	// }

	logger.WithField("idpAccount", account).Debug("building provider")
	provider, err := saml2aws.NewSAMLClient(account)
	if err != nil {
		return nil, errors.Wrap(err, "Error building IdP client.")
	}
	err = provider.Validate(loginDetails)
	if err != nil {
		return nil, errors.Wrap(err, "Error validating login details.")
	}

	var samlAssertion string
	log.Printf("Authenticating as %s ...", loginDetails.Username)

	samlAssertion, err = provider.Authenticate(loginDetails)
	if err != nil {
		return nil, errors.Wrap(err, "Error authenticating to IdP.")
	}

	role, err := selectAwsRole(samlAssertion, account)
	if err != nil {
		return nil, errors.Wrap(err, "Failed to assume role. Please check whether you are permitted to assume the given role for the AWS service.")
	}

	log.Println("Selected role:", role.RoleARN)

	awsCreds, err := loginToStsUsingRole(account, role, samlAssertion)
	if err != nil {
		return nil, errors.Wrap(err, "Error logging into AWS role using SAML assertion.")
	}

	return awsCreds, nil
}

func selectAwsRole(samlAssertion string, account *cfg.IDPAccount) (*saml2aws.AWSRole, error) {
	data, err := b64.StdEncoding.DecodeString(samlAssertion)
	if err != nil {
		return nil, errors.Wrap(err, "Error decoding SAML assertion.")
	}

	roles, err := saml2aws.ExtractAwsRoles(data)
	if err != nil {
		return nil, errors.Wrap(err, "Error parsing AWS roles.")
	}

	if len(roles) == 0 {
		log.Println("No roles to assume.")
		log.Println("Please check you are permitted to assume roles for the AWS service.")
		os.Exit(1)
	}

	awsRoles, err := saml2aws.ParseAWSRoles(roles)
	if err != nil {
		return nil, errors.Wrap(err, "Error parsing AWS roles.")
	}

	return resolveRole(awsRoles, samlAssertion, account)
}

func resolveRole(awsRoles []*saml2aws.AWSRole, samlAssertion string, account *cfg.IDPAccount) (*saml2aws.AWSRole, error) {
	var role = new(saml2aws.AWSRole)

	if len(awsRoles) == 1 {
		if account.RoleARN != "" {
			return saml2aws.LocateRole(awsRoles, account.RoleARN)
		}
		return awsRoles[0], nil
	} else if len(awsRoles) == 0 {
		return nil, errors.New("No roles available.")
	}

	samlAssertionData, err := b64.StdEncoding.DecodeString(samlAssertion)
	if err != nil {
		return nil, errors.Wrap(err, "Error decoding SAML assertion.")
	}

	aud, err := saml2aws.ExtractDestinationURL(samlAssertionData)
	if err != nil {
		return nil, errors.Wrap(err, "Error parsing destination URL.")
	}

	awsAccounts, err := saml2aws.ParseAWSAccounts(aud, samlAssertion)
	if err != nil {
		return nil, errors.Wrap(err, "Error parsing AWS role accounts.")
	}
	if len(awsAccounts) == 0 {
		return nil, errors.New("No accounts available.")
	}

	saml2aws.AssignPrincipals(awsRoles, awsAccounts)

	if account.RoleARN != "" {
		return saml2aws.LocateRole(awsRoles, account.RoleARN)
	}

	for {
		role, err = saml2aws.PromptForAWSRoleSelection(awsAccounts)
		if err == nil {
			break
		}
		log.Println("Error selecting role. Try again.")
	}

	return role, nil
}

func loginToStsUsingRole(account *cfg.IDPAccount, role *saml2aws.AWSRole, samlAssertion string) (*awsconfig.AWSCredentials, error) {

	sess, err := session.NewSession(&aws.Config{
		Region: &account.Region,
	})
	if err != nil {
		return nil, errors.Wrap(err, "Failed to create session.")
	}

	svc := sts.New(sess)

	params := &sts.AssumeRoleWithSAMLInput{
		PrincipalArn:    aws.String(role.PrincipalARN), // Required
		RoleArn:         aws.String(role.RoleARN),      // Required
		SAMLAssertion:   aws.String(samlAssertion),     // Required
		DurationSeconds: aws.Int64(int64(account.SessionDuration)),
	}

	log.Println("Requesting AWS credentials using SAML assertion.")

	resp, err := svc.AssumeRoleWithSAML(params)
	if err != nil {
		return nil, errors.Wrap(err, "Error retrieving STS credentials using SAML.")
	}

	return &awsconfig.AWSCredentials{
		AWSAccessKey:     aws.StringValue(resp.Credentials.AccessKeyId),
		AWSSecretKey:     aws.StringValue(resp.Credentials.SecretAccessKey),
		AWSSessionToken:  aws.StringValue(resp.Credentials.SessionToken),
		AWSSecurityToken: aws.StringValue(resp.Credentials.SessionToken),
		PrincipalARN:     aws.StringValue(resp.AssumedRoleUser.Arn),
		Expires:          resp.Credentials.Expiration.Local(),
		Region:           account.Region,
	}, nil
}

// CredentialsToCredentialProcess
// Returns a Json output that is compatible with the AWS credential_process
// https://github.com/awslabs/awsprocesscreds
func CredentialsToCredentialProcess(awsCreds *awsconfig.AWSCredentials) (string, error) {

	type AWSCredentialProcess struct {
		Version         int
		AccessKeyId     string
		SecretAccessKey string
		SessionToken    string
		Expiration      string
	}

	cred_process := AWSCredentialProcess{
		Version:         1,
		AccessKeyId:     awsCreds.AWSAccessKey,
		SecretAccessKey: awsCreds.AWSSecretKey,
		SessionToken:    awsCreds.AWSSessionToken,
		Expiration:      awsCreds.Expires.Format(time.RFC3339),
	}

	p, err := json.Marshal(cred_process)
	if err != nil {
		return "", errors.Wrap(err, "Error while marshalling the credential process.")
	}
	return string(p), nil
}

// PrintCredentialProcess Prints a Json output that is compatible with the AWS credential_process
// https://github.com/awslabs/awsprocesscreds
func PrintCredentialProcess(awsCreds *awsconfig.AWSCredentials) error {
	jsonData, err := CredentialsToCredentialProcess(awsCreds)
	if err == nil {
		fmt.Println(jsonData)
	}
	return err
}