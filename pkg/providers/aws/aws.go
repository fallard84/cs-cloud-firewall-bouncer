package aws

import (
	"fmt"
	"strings"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/networkfirewall"
	"github.com/aws/aws-sdk-go/service/networkfirewall/networkfirewalliface"
	"github.com/fallard84/cs-cloud-firewall-bouncer/pkg/models"
	"github.com/sirupsen/logrus"
)

type Client struct {
	svc               networkfirewalliface.NetworkFirewallAPI
	capacity          int
	firewallPolicy    string
	ruleGroupPriority int
}

const (
	providerName             = "aws"
	defaultCapacity          = 1000
	defaultRuleGroupPriority = 1
)

func (c *Client) MaxSourcesPerRule() int {
	return c.capacity
}
func (c *Client) MaxRules() int {
	return 1
}

func (c *Client) GetProviderName() string {
	return providerName
}

var log *logrus.Entry

func init() {
	log = logrus.WithField("provider", providerName)
}

func assignDefault(config *models.AWSConfig) {
	if config.Capacity == 0 {
		config.Capacity = defaultCapacity
	}
	if config.RuleGroupPriority == 0 {
		config.RuleGroupPriority = defaultRuleGroupPriority
	}
}

// NewClient creates a new AWS client
func NewClient(config *models.AWSConfig) (*Client, error) {
	sess := session.Must(session.NewSessionWithOptions(session.Options{
		SharedConfigState: session.SharedConfigEnable,
		Config:            aws.Config{Region: aws.String(config.Region)},
	}))
	_, err := sess.Config.Credentials.Get()
	if err != nil {
		log.Errorf("error while loading credentials: %s", err)
	}
	svc := networkfirewall.New(sess)
	assignDefault(config)

	return &Client{
		svc:               svc,
		capacity:          config.Capacity,
		firewallPolicy:    config.FirewallPolicy,
		ruleGroupPriority: config.RuleGroupPriority,
	}, nil
}

func (c *Client) getFirewallPolicy() *networkfirewall.DescribeFirewallPolicyOutput {
	res, err := c.svc.DescribeFirewallPolicy(&networkfirewall.DescribeFirewallPolicyInput{
		FirewallPolicyName: &c.firewallPolicy,
	})
	if err != nil {
		log.Panicf("ccan't get firewall policy: %s", err.Error())
	}
	return res
}

func (c *Client) addRuleToFirewallPolicy(ruleARN string, fp *networkfirewall.DescribeFirewallPolicyOutput) {
	priority := int64(1)
	newRuleRef := networkfirewall.StatelessRuleGroupReference{
		Priority:    &priority,
		ResourceArn: &ruleARN,
	}
	rules := append(fp.FirewallPolicy.StatelessRuleGroupReferences, &newRuleRef)
	fp.FirewallPolicy.SetStatelessRuleGroupReferences(rules)

	input := networkfirewall.UpdateFirewallPolicyInput{
		FirewallPolicyArn: fp.FirewallPolicyResponse.FirewallPolicyArn,
		FirewallPolicy:    fp.FirewallPolicy,
		UpdateToken:       fp.UpdateToken,
	}
	_, err := c.svc.UpdateFirewallPolicy(&input)
	if err != nil {
		log.Fatalf("Unable to update firewall policy %v: %v", fp.FirewallPolicyResponse.FirewallPolicyName, err.Error())
	}
	log.Infof("Update of firewall policy successful")
}

func ConvertSourceMapToAWSSlice(sources map[string]bool) []*networkfirewall.Address {
	slice := []*networkfirewall.Address{}
	for source := range sources {
		log.Debugf("key: %s", source)
		sourceToAppend := source
		slice = append(slice, &networkfirewall.Address{AddressDefinition: &sourceToAppend})
	}
	return slice
}

func (c *Client) GetRules(ruleNamePrefix string) ([]*models.FirewallRule, error) {

	fp := c.getFirewallPolicy()

	var rules []*models.FirewallRule
	for _, ruleGroup := range fp.FirewallPolicy.StatelessRuleGroupReferences {
		if strings.Contains(*ruleGroup.ResourceArn, ruleNamePrefix) {
			res, err := c.svc.DescribeRuleGroup(&networkfirewall.DescribeRuleGroupInput{
				RuleGroupArn: ruleGroup.ResourceArn,
			})
			if err != nil {
				return nil, fmt.Errorf("can't describe rule  %s: %s", *ruleGroup.ResourceArn, err.Error())
			}
			if *res.RuleGroupResponse.RuleGroupStatus == networkfirewall.ResourceStatusDeleting {
				log.Debugf("Skipping rule %s because it is being deleted", *res.RuleGroupResponse.RuleGroupName)
				break
			}
			var sources []string
			log.Debugf("Found rule %s", *res.RuleGroupResponse.RuleGroupName)
			if len(res.RuleGroup.RulesSource.StatelessRulesAndCustomActions.StatelessRules) > 0 {
				for _, source := range res.RuleGroup.RulesSource.StatelessRulesAndCustomActions.StatelessRules[0].RuleDefinition.MatchAttributes.Sources {
					sources = append(sources, *source.AddressDefinition)
				}
			}
			log.Infof("%s  (%d rules): %#v", *res.RuleGroupResponse.RuleGroupName, len(sources), sources)
			rule := models.FirewallRule{
				Name:         *res.RuleGroupResponse.RuleGroupName,
				SourceRanges: models.ConvertSourceRangesSliceToMap(sources),
			}
			rules = append(rules, &rule)
		}
	}

	return rules, nil
}

func (c *Client) CreateRule(rule *models.FirewallRule) error {
	log.Infof("Creating rule group %v with %#v", rule.Name, rule.SourceRanges)
	ruleType := networkfirewall.RuleGroupTypeStateless

	awsRule := networkfirewall.StatelessRule{
		Priority: aws.Int64(int64(c.ruleGroupPriority)),
		RuleDefinition: &networkfirewall.RuleDefinition{
			MatchAttributes: &networkfirewall.MatchAttributes{
				Sources: ConvertSourceMapToAWSSlice(rule.SourceRanges),
			},
			Actions: []*string{aws.String("aws:drop")},
		},
	}

	rg, err := c.svc.CreateRuleGroup(&networkfirewall.CreateRuleGroupInput{
		Capacity:      aws.Int64(int64(c.capacity)),
		Description:   aws.String("Blocklist generated by CrowdSec Cloud Firewall Bouncer"),
		RuleGroupName: &rule.Name,
		RuleGroup: &networkfirewall.RuleGroup{
			RulesSource: &networkfirewall.RulesSource{
				StatelessRulesAndCustomActions: &networkfirewall.StatelessRulesAndCustomActions{
					StatelessRules: []*networkfirewall.StatelessRule{&awsRule},
				},
			},
		},
		Type: &ruleType,
	})
	if err != nil {
		return err
	}
	fp := c.getFirewallPolicy()
	c.addRuleToFirewallPolicy(*rg.RuleGroupResponse.RuleGroupArn, fp)

	log.Infof("Creation successful")
	return nil
}

// DeleteRule implementation for AWS only empties the rule group instead of deleting it
// This is to avoid removing the rule group from  the firewall policy
func (c *Client) DeleteRule(rule *models.FirewallRule) error {
	log.Infof("Deleting firewall rule %s", rule.Name)
	return c.PatchRule(rule)
}

func (c *Client) PatchRule(rule *models.FirewallRule) error {
	log.Infof("Patching firewall rule %v with %#v", rule.Name, rule.SourceRanges)
	ruleType := networkfirewall.RuleGroupTypeStateless
	res, err := c.svc.DescribeRuleGroup(&networkfirewall.DescribeRuleGroupInput{
		RuleGroupName: &rule.Name,
		Type:          &ruleType,
	})
	if err != nil {
		return err
	}
	res.RuleGroup.RulesSource.StatelessRulesAndCustomActions.StatelessRules[0].RuleDefinition.MatchAttributes.Sources = ConvertSourceMapToAWSSlice(rule.SourceRanges)

	input := networkfirewall.UpdateRuleGroupInput{
		RuleGroupName: &rule.Name,
		Type:          &ruleType,
		RuleGroup:     res.RuleGroup,
		UpdateToken:   res.UpdateToken,
	}
	_, err = c.svc.UpdateRuleGroup(&input)
	if err != nil {
		log.Fatalf("Unable to patch firewall rule %v: %v", rule.Name, err.Error())
	}
	log.Infof("Patch successful")
	return nil
}
