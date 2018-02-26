package obmc

import (
	"context"
	"sync"

	domain "github.com/superchalupa/go-redfish/redfishresource"

	eh "github.com/looplab/eventhorizon"
)

var (
	OBMC_SystemPlugin = domain.PluginType("obmc_system")
)

// OCP Profile Redfish System object

type systemService struct {
	serviceMu sync.Mutex
}

func NewSystemService(ctx context.Context) (*systemService, error) {
	return &systemService{}, nil
}

// satisfy the plugin interface so we can list ourselves as a plugin in our @meta
func (s *systemService) PluginType() domain.PluginType { return OBMC_SystemPlugin }

func (s *systemService) RefreshProperty(
	ctx context.Context,
	agg *domain.RedfishResourceAggregate,
	rrp *domain.RedfishResourceProperty,
	method string,
	meta map[string]interface{},
	body interface{},
) {
	s.serviceMu.Lock()
	defer s.serviceMu.Unlock()

	rrp.Value = "NOT IMPLEMENTED YET"
}

func (s *systemService) AddOBMCSystemResource(ctx context.Context, ch eh.CommandHandler) {
	ch.HandleCommand(
		context.Background(),
		&domain.CreateRedfishResource{
			ID:          eh.NewUUID(),
			Collection:  false,
			ResourceURI: "/redfish/v1/Systems/2M220100SL",
			Type:        "#ComputerSystem.v1_1_0.ComputerSystem",
			Context:     "/redfish/v1/$metadata#ComputerSystem.ComputerSystem",
			Privileges: map[string]interface{}{
				"GET":    []string{"Login"},
				"POST":   []string{}, // cannot create sub objects
				"PUT":    []string{"ConfigureManager"},
				"PATCH":  []string{"ConfigureManager"},
				"DELETE": []string{}, // can't be deleted
			},
			Properties: map[string]interface{}{
				"Id":           "2M220100SL",
				"Name":         "Catfish System",
				"SystemType":   "Physical",
				"AssetTag":     "CATFISHASSETTAG",
				"Manufacturer": "CatfishManufacturer",
				"Model":        "YellowCat1000",
				"SerialNumber": "2M220100SL",
				"SKU":          "",
				"PartNumber":   "",
				"Description":  "Catfish Implementation Recipe of simple scale-out monolithic server",
				"UUID":         "00000000-0000-0000-0000-000000000000",
				"HostName":     "catfishHostname",
				"PowerState":   "On",
				"BiosVersion":  "X00.1.2.3.4(build-23)",
				"Status": map[string]interface{}{
					"State":  "Enabled",
					"Health": "OK",
				},
				"IndicatorLED": "Off",
				"Boot": map[string]interface{}{
					"BootSourceOverrideEnabled":    "Once",
					"BootSourceOverrideMode":       "UEFI",
					"UefiTargetBootSourceOverride": "uefiDevicePath",
					"BootSourceOverrideTarget":     "Pxe",
					"BootSourceOverrideTarget@Redfish.AllowableValues": []string{
						"None",
						"Pxe",
						"Usb",
						"Hdd",
						"BiosSetup",
						"UefiTarget",
						"UefiHttp",
					},
				},
				"LogServices": map[string]interface{}{
					"@odata.id": "/redfish/v1/Systems/2M220100SL/LogServices",
				},
				"Links": map[string]interface{}{
					"Chassis": []map[string]interface{}{
						map[string]interface{}{
							"@odata.id": "/redfish/v1/Chassis/A33",
						},
					},
					"ManagedBy": []map[string]interface{}{
						map[string]interface{}{
							"@odata.id": "/redfish/v1/Managers/bmc",
						},
					},
					"Oem": map[string]interface{}{},
				},
				"Actions": map[string]interface{}{
					"#ComputerSystem.Reset": map[string]interface{}{
						"target": "/redfish/v1/Systems/2M220100SL/Actions/ComputerSystem.Reset",
						"ResetType@Redfish.AllowableValues": []string{
							"On",
							"ForceOff",
							"GracefulShutdown",
							"ForceRestart",
							"Nmi",
							"GracefulRestart",
							"ForceOn",
						},
					},
				},
			}})
}
