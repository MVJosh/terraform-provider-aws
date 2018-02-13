package aws

import (
	"fmt"
	"log"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/arn"
	"github.com/aws/aws-sdk-go/service/directconnect"
	"github.com/hashicorp/terraform/helper/resource"
	"github.com/hashicorp/terraform/helper/schema"
	"github.com/hashicorp/terraform/helper/validation"
)

// Schemas common to all (public/private, hosted or not) virtual interfaces.
var dxVirtualInterfaceSchemaWithTags = mergeSchemas(
	dxVirtualInterfaceSchema,
	map[string]*schema.Schema{
		"tags": tagsSchema(),
	},
)
var dxVirtualInterfaceSchema = map[string]*schema.Schema{
	"arn": {
		Type:     schema.TypeString,
		Computed: true,
	},
	"connection_id": {
		Type:     schema.TypeString,
		Required: true,
		ForceNew: true,
	},
	"name": {
		Type:     schema.TypeString,
		Required: true,
		ForceNew: true,
	},
	"vlan": {
		Type:         schema.TypeInt,
		Required:     true,
		ForceNew:     true,
		ValidateFunc: validation.IntBetween(1, 4094),
	},
	"bgp_asn": {
		Type:     schema.TypeInt,
		Required: true,
		ForceNew: true,
	},
	"bgp_auth_key": {
		Type:     schema.TypeString,
		Optional: true,
		Computed: true,
		ForceNew: true,
	},
	"address_family": {
		Type:         schema.TypeString,
		Required:     true,
		ForceNew:     true,
		ValidateFunc: validation.StringInSlice([]string{directconnect.AddressFamilyIpv4, directconnect.AddressFamilyIpv6}, false),
	},
	"customer_address": {
		Type:     schema.TypeString,
		Optional: true,
		Computed: true,
		ForceNew: true,
	},
	"amazon_address": {
		Type:     schema.TypeString,
		Optional: true,
		Computed: true,
		ForceNew: true,
	},
}

func isNoSuchDxVirtualInterfaceErr(err error) bool {
	return isAWSErr(err, "DirectConnectClientException", "does not exist")
}

func dxVirtualInterfaceRead(d *schema.ResourceData, meta interface{}) (*directconnect.VirtualInterface, error) {
	conn := meta.(*AWSClient).dxconn

	resp, state, err := dxVirtualInterfaceStateRefresh(conn, d.Id())()
	if err != nil {
		return nil, fmt.Errorf("Error reading Direct Connect virtual interface: %s", err.Error())
	}
	terminalStates := map[string]bool{
		directconnect.VirtualInterfaceStateDeleted:  true,
		directconnect.VirtualInterfaceStateDeleting: true,
		directconnect.VirtualInterfaceStateRejected: true,
	}
	if _, ok := terminalStates[state]; ok {
		log.Printf("[WARN] Direct Connect virtual interface (%s) not found, removing from state", d.Id())
		d.SetId("")
		return nil, nil
	}

	return resp.(*directconnect.VirtualInterface), nil
}

func dxVirtualInterfaceUpdate(d *schema.ResourceData, meta interface{}) error {
	conn := meta.(*AWSClient).dxconn

	arn := arn.ARN{
		Partition: meta.(*AWSClient).partition,
		Region:    meta.(*AWSClient).region,
		Service:   "directconnect",
		AccountID: meta.(*AWSClient).accountid,
		Resource:  fmt.Sprintf("dxvif/%s", d.Id()),
	}.String()
	if err := setTagsDX(conn, d, arn); err != nil {
		return err
	}

	return nil
}

func dxVirtualInterfaceDelete(d *schema.ResourceData, meta interface{}) error {
	conn := meta.(*AWSClient).dxconn

	log.Printf("[DEBUG] Deleting Direct Connect virtual interface: %s", d.Id())
	_, err := conn.DeleteVirtualInterface(&directconnect.DeleteVirtualInterfaceInput{
		VirtualInterfaceId: aws.String(d.Id()),
	})
	if err != nil {
		if isNoSuchDxVirtualInterfaceErr(err) {
			return nil
		}
		return fmt.Errorf("Error deleting Direct Connect virtual interface: %s", err.Error())
	}

	deleteStateConf := &resource.StateChangeConf{
		Pending: []string{
			directconnect.VirtualInterfaceStateAvailable,
			directconnect.VirtualInterfaceStateConfirming,
			directconnect.VirtualInterfaceStateDeleting,
			directconnect.VirtualInterfaceStateDown,
			directconnect.VirtualInterfaceStatePending,
			directconnect.VirtualInterfaceStateRejected,
			directconnect.VirtualInterfaceStateVerifying,
		},
		Target: []string{
			directconnect.VirtualInterfaceStateDeleted,
		},
		Refresh:    dxVirtualInterfaceStateRefresh(conn, d.Id()),
		Timeout:    d.Timeout(schema.TimeoutDelete),
		Delay:      10 * time.Second,
		MinTimeout: 5 * time.Second,
	}
	_, err = deleteStateConf.WaitForState()
	if err != nil {
		return fmt.Errorf("Error waiting for Direct Connect virtual interface (%s) to be deleted: %s", d.Id(), err)
	}

	return nil
}

func dxVirtualInterfaceStateRefresh(conn *directconnect.DirectConnect, vifId string) resource.StateRefreshFunc {
	return func() (interface{}, string, error) {
		resp, err := conn.DescribeVirtualInterfaces(&directconnect.DescribeVirtualInterfacesInput{
			VirtualInterfaceId: aws.String(vifId),
		})
		if err != nil {
			if isNoSuchDxVirtualInterfaceErr(err) {
				return nil, directconnect.VirtualInterfaceStateDeleted, nil
			}
			return nil, "", err
		}

		if len(resp.VirtualInterfaces) < 1 {
			return nil, directconnect.ConnectionStateDeleted, nil
		}
		vif := resp.VirtualInterfaces[0]
		return vif, aws.StringValue(vif.VirtualInterfaceState), nil
	}
}

func dxVirtualInterfaceWaitUntilAvailable(d *schema.ResourceData, conn *directconnect.DirectConnect, pending, target []string) error {
	stateConf := &resource.StateChangeConf{
		Pending:    pending,
		Target:     target,
		Refresh:    dxVirtualInterfaceStateRefresh(conn, d.Id()),
		Timeout:    d.Timeout(schema.TimeoutCreate),
		Delay:      10 * time.Second,
		MinTimeout: 5 * time.Second,
	}
	if _, err := stateConf.WaitForState(); err != nil {
		return fmt.Errorf("Error waiting for Direct Connect virtual interface %s to become available: %s", d.Id(), err.Error())
	}

	return nil
}

// Attributes common to public VIFs and creator side of hosted public VIFs.
func dxPublicVirtualInterfaceAttributes(d *schema.ResourceData, meta interface{}, vif *directconnect.VirtualInterface) error {
	if err := dxVirtualInterfaceAttributes(d, meta, vif); err != nil {
		return err
	}
	d.Set("route_filter_prefixes", flattenDxRouteFilterPrefixes(vif.RouteFilterPrefixes))

	return nil
}

// Attributes common to private VIFs and creator side of hosted private VIFs.
func dxPrivateVirtualInterfaceAttributes(d *schema.ResourceData, meta interface{}, vif *directconnect.VirtualInterface) error {
	return dxVirtualInterfaceAttributes(d, meta, vif)
}

// Attributes common to public/private VIFs and creator side of hosted public/private VIFs.
func dxVirtualInterfaceAttributes(d *schema.ResourceData, meta interface{}, vif *directconnect.VirtualInterface) error {
	if err := dxVirtualInterfaceArnAttribute(d, meta); err != nil {
		return err
	}

	d.Set("connection_id", vif.ConnectionId)
	d.Set("name", vif.VirtualInterfaceName)
	d.Set("vlan", vif.Vlan)
	d.Set("bgp_asn", vif.Asn)
	d.Set("bgp_auth_key", vif.AuthKey)
	d.Set("address_family", vif.AddressFamily)
	d.Set("customer_address", vif.CustomerAddress)
	d.Set("amazon_address", vif.AmazonAddress)

	return nil
}

func dxVirtualInterfaceArnAttribute(d *schema.ResourceData, meta interface{}) error {
	arn := arn.ARN{
		Partition: meta.(*AWSClient).partition,
		Region:    meta.(*AWSClient).region,
		Service:   "directconnect",
		AccountID: meta.(*AWSClient).accountid,
		Resource:  fmt.Sprintf("dxvif/%s", d.Id()),
	}.String()
	d.Set("arn", arn)

	return nil
}

func expandDxRouteFilterPrefixes(cfg []interface{}) []*directconnect.RouteFilterPrefix {
	prefixes := make([]*directconnect.RouteFilterPrefix, len(cfg), len(cfg))
	for i, p := range cfg {
		prefix := &directconnect.RouteFilterPrefix{
			Cidr: aws.String(p.(string)),
		}
		prefixes[i] = prefix
	}
	return prefixes
}

func flattenDxRouteFilterPrefixes(prefixes []*directconnect.RouteFilterPrefix) *schema.Set {
	out := make([]interface{}, 0)
	for _, prefix := range prefixes {
		out = append(out, aws.StringValue(prefix.Cidr))
	}
	return schema.NewSet(schema.HashString, out)
}
