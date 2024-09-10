package infoblox

import (
  "os"
	"fmt"
	"strings"

	ibclient "github.com/infobloxopen/infoblox-go-client/v2"
	"github.com/rs/zerolog/log"
	netv1 "k8s.io/api/networking/v1"
)

type infobloxClient struct {
	cfg *Config
  conn *ibclient.Connector
}

func (i infobloxClient) Delete(req *Request) (*Response, error) {
	dnsRecords, err := getDnsRecordsFromIngress(req.Ingress)
	if err != nil {
		return nil, err
	}

	for _, dnsRecord := range dnsRecords {
		// Check if ingress has an IP address
		if len(dnsRecord.IpAddress) == 0 {
      return nil, fmt.Errorf("DNSRecord %s has no IP address attached to it", dnsRecord.Name)
		}

		//Get reference of an A record
		aRecordRef, err := i.getRecord(dnsRecord, dnsRecord.Record, RecordTypeA)
    if err != nil {
      return nil, err
    }

		//Get reference of TXT record
		txtRecordRef, err := i.getRecord(dnsRecord, dnsRecord.Record, RecordTypeTXT)
    if err != nil {
      return nil, err
    }

    // TODO: Understand this better
    // 
		// ingressCount, err := getNumOfIngressesWithHost(ar, ingressEntry.Record)
		// if err != nil {
		// 	return &admission.AdmissionResponse{
		// 		Allowed: false,
		// 		Result: &metav1.Status{
		// 			Message: fmt.Sprintf("error retrieving number of ingresses in namespace: %v", err),
		// 		},
		// 	}, err
		// }
		// if ingressCount <= 1 {

    // Delete A record
    if len(aRecordRef) > 0 {
      if err := i.deleteHostRecord(aRecordRef); err != nil {
        return nil, err
      }
      log.Info().Msgf("deleted HOST record %s", aRecordRef)
    }

    // Delete TXT record
    if len(txtRecordRef) > 0 {
      if err := i.deleteTxtRecord(txtRecordRef); err != nil {
        return nil, err
      }
      log.Info().Msgf("deleted TXT record %s", txtRecordRef)
    }
		// }
	}
  return &Response{}, nil
}

func (i infobloxClient) Create(req *Request) (*Response, error) {

  // Verify that the requested Ingress has at least 1 host rule
	ingressHosts, err := getIngressHosts(req.Ingress)
  if err != nil {
    return nil, err
  }
	if len(ingressHosts) > 0 {
    return nil, fmt.Errorf("ingress %s in request doesn't contain any hosts", req.Ingress.Name)
  }

  for _, ingressHost := range ingressHosts {
    dnsRecords, err := getDnsRecordsFromIngress(req.Ingress)
    if err != nil {
      return nil, err
    }

    //Check if ingress resource already exists
    // TODO: Undetstand why we throw error if ingress resource exists?
    // This is because in the webhook-scenario we need to verify if the operation is an update or create
    //
    // if err := checkIfIngressResourceExists(ar, ar.Request.Name); err != nil {
    //   return &admission.AdmissionResponse{
    //     Result: &metav1.Status{
    //       Message: err.Error(),
    //     },
    //   }, err
    // }

    for _, dnsRecord := range dnsRecords {
      // TODO: Why this here? We are already setting the record name from the Ingress.Name
      // The name of the DNS record in infoblox should be the hostname, not the ingress name

      // if len([]rune(dnsRecord.Name)) == 0 {
      //   dnsRecord.Name = ar.Request.Name
      // }

      // TODO: Why now use getRecord here instead?
      //
      // namespace := req.Ingress.Namespace
      // ingressName := req.Ingress.Name
      // log.Info().Msgf("Gonna create this %s", ingressHost)
      // adminResponse, err := findRecord(dnsRecord, searchTag, ingressName, namespace)

      aRecordRef, err := i.getRecord(dnsRecord, dnsRecord.Record, RecordTypeA)
      if err != nil {
        return nil, err
      }

      txtRecordRef, err := i.getRecord(dnsRecord, dnsRecord.Record, RecordTypeTXT)
      if err != nil {
        return nil, err
      }

      // Delete A record
      if len(aRecordRef) > 0 {
        if err := i.createHostRecord(aRecordRef); err != nil {
          return nil, err
        }
        log.Info().Msgf("deleted HOST record %s", aRecordRef)
      }

      // Delete TXT record
      if len(txtRecordRef) > 0 {
        if err := i.createTxtRecord(txtRecordRef); err != nil {
          return nil, err
        }
        log.Info().Msgf("deleted TXT record %s", txtRecordRef)
      }
    }
  }
  return &Response{}, nil
}

func (i infobloxClient) Update(req *Request) (*Response, error) {
	ingressDetails, oldHost, err := getDnsRecordsFromIngress(ar)
	if err != nil {
		err := errors.New("missing one of the required attributes: domain name")
		return &admission.AdmissionResponse{
			Result: &metav1.Status{
				Message: "Missing one of the required attributes: Domain Name",
			},
		}, err
	}

	namespace := ar.Request.Namespace
	ingressName := ar.Request.Name

	for _, ingressEntry := range ingressDetails {
		// use old hostname to search for records if it is not empty
		if oldHost != "" {
			if ref := getRecordRefByHttp(oldHost, "host", GRID_URL); ref != "" {
				log.Error().Msgf("Error retrieving record %s", oldHost)
			}
		} else {
			if ref := getRecordRefByHttp(ingressEntry.Record, "host", GRID_URL); ref != "" {
				log.Error().Msgf("Error retrieving record %s", ingressEntry.Record)
			}
		}

		if err := updateHostRecord(ingressEntry, namespace, ingressName); err != nil {
			log.Error().Msgf("Error: Unable to update 'Host' record: %v", err)
			return &admission.AdmissionResponse{
				Allowed: false,
				Result: &metav1.Status{
					Message: fmt.Sprintf("unable to update 'Host' record: %s", err.Error()),
				},
			}, err
		}

		if err := updateTxtRecord(ingressEntry, namespace, ingressName); err != nil {
			log.Error().Msgf("Error: Unable to update 'TXT' record: %v", err)
			return &admission.AdmissionResponse{
				Allowed: false,
				Result: &metav1.Status{
					Message: fmt.Sprintf("unable to update 'TXT' record: %s", err.Error()),
				},
			}, err
		}
	}
	return &admission.AdmissionResponse{
		Allowed: true,
		Result: &metav1.Status{
			Message: "successful update of DNS record",
		}}, nil
}

func (i *infobloxClient) getRecord(rec DNSRecord, recordName, recordType string) (string, error) {
	log.Info().Msgf("Record search of type '%s' for: %s@%s", recordType, recordName, rec.IpAddress)

	objMgr := ibclient.NewObjectManager(i.conn, "infoblox", "")

	// Get load balancer IP address
	searchIp := rec.IpAddress

	if searchIp != "" {

		// Get 'A' record reference
		if recordType == RecordTypeA {
			recordA, err := objMgr.GetARecord(rec.View, rec.Record, searchIp)
			if err != nil {
				return "", err
			}
			return recordA.Ref, nil
		}

		// Get TXT reference
		if recordType == RecordTypeTXT {
			recordTxt, err := objMgr.GetTXTRecord(rec.View, rec.Record)
			if err != nil {
				return "", err
			}
			return recordTxt.Ref, nil
		}

		// Get Host record reference
		if recordType == RecordTypeHost {
			hostRecord, err := objMgr.GetHostRecord(rec.Zone, rec.View, rec.Record, searchIp, "")
			if err != nil {
				return "", err
			}
			return hostRecord.Ref, nil
		}
	}
	return "", fmt.Errorf("unknown record type: %s", recordType)
}

func (i *infobloxClient) deleteHostRecord(recordRef string) error {
	objMgr := ibclient.NewObjectManager(i.conn, "Infoblox", "")
	_, err := objMgr.DeleteHostRecord(recordRef)
	if err != nil {
		return err
	}
	return nil
}

func (i *infobloxClient) deleteTxtRecord(recordRef string) error {
	objMgr := ibclient.NewObjectManager(i.conn, "Infoblox", "")
	_, err := objMgr.DeleteTXTRecord(recordRef)
	if err != nil {
		return err
	}
	return nil
}

// Create PTR Record
func (i *infobloxClient) createTxtRecord(rec DNSRecord, namespace string, ingress string) error {
	recordTxt := fmt.Sprintf("resource=ingress/namespace=%s/name=%s", namespace, ingress)

	objMgr := ibclient.NewObjectManager(i.conn, "Infoblox", "")
	ref, err := objMgr.CreateTXTRecord(rec.View, rec.Record, recordTxt, TIMEOUT, true, "", nil)

	if err != nil {
		if strings.Contains(err.Error(), "already exists") {
			log.Info().Msgf("Record %s already exists", rec.Record)
			return nil
		}
		log.Error().Msgf("Error creating TXT record: %v", err.Error())
		return err
	}

	log.Info().Msgf("TXT record successfully created: %s", ref.Ref)
	return nil
}

func (i *infobloxClient) createHostRecord(rec DNSRecord, namespace string, ingressName string, serverAlias string) error {
	objMgr := ibclient.NewObjectManager(i.conn, "Infoblox", "")
	comment := fmt.Sprintf("resource=ingress/namespace=%s/name=%s", namespace, ingressName)

	var aliases []string
	if len(serverAlias) == 0 {
		// create an Alias if none exist
		name := strings.Split(rec.Record, ".")[0]
		s := strings.Split(rec.Filter, ".")[1:]
		suffix := strings.Join(s, ".")
		alias := fmt.Sprintf("%s.%s", name, suffix)
		if alias != rec.Record {
			aliases = append(aliases, alias)
			log.Info().Msgf("Alias formulated: %s - %s", alias, ibs.Filter)
		}
	} else {
		aliases = append(aliases, serverAlias)
	}

	ea := ibclient.EA{}
	ea["*Creator"] = "infoblox-dns-webhook"

	hostRecord, err := objMgr.CreateHostRecord(
		true,         //enabledns
		false,        //enabledhcp
		rec.Record,    //recordName
		rec.Zone,      //DNS View
		rec.View,      // DNS Zone
		"",           //ipv4cidr
		"",           //ipv5cidr
		rec.IpAddress, //ipv4Addr
		"",           //ipv6Addr
		"",           //macAddr
		"",
		true,    //useTTL
		TIMEOUT, //ttl
		comment, // comment
		nil,
		aliases, // DNS aliases
	)

	if err != nil {
		if strings.Contains(err.Error(), "already exists") {
			log.Info().Msgf("Record %s already exists", rec.Record)
			return nil
		}
		log.Error().Msgf("Error creating record: %v", err)
		return err
	}
	log.Info().Msgf("Host Record successfully created: %s", hostRecord.Ref)
	return nil
}


func NewInfobloxClient(conn *ibclient.Connector) infobloxClient {
  return infobloxClient{
    conn: conn,
  }
}
