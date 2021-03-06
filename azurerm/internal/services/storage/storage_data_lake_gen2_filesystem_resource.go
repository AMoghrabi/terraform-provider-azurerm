package storage

import (
	"context"
	"fmt"
	"log"
	"regexp"
	"time"

	"github.com/hashicorp/terraform-plugin-sdk/helper/schema"
	"github.com/hashicorp/terraform-plugin-sdk/helper/validation"
	"github.com/terraform-providers/terraform-provider-azurerm/azurerm/helpers/tf"
	"github.com/terraform-providers/terraform-provider-azurerm/azurerm/internal/clients"
	"github.com/terraform-providers/terraform-provider-azurerm/azurerm/internal/services/storage/parse"
	"github.com/terraform-providers/terraform-provider-azurerm/azurerm/internal/services/storage/validate"
	"github.com/terraform-providers/terraform-provider-azurerm/azurerm/internal/timeouts"
	"github.com/terraform-providers/terraform-provider-azurerm/azurerm/utils"
	"github.com/tombuildsstuff/giovanni/storage/2019-12-12/datalakestore/filesystems"
	"github.com/tombuildsstuff/giovanni/storage/2019-12-12/datalakestore/paths"
	"github.com/tombuildsstuff/giovanni/storage/accesscontrol"
)

func resourceStorageDataLakeGen2FileSystem() *schema.Resource {
	return &schema.Resource{
		Create: resourceStorageDataLakeGen2FileSystemCreate,
		Read:   resourceStorageDataLakeGen2FileSystemRead,
		Update: resourceStorageDataLakeGen2FileSystemUpdate,
		Delete: resourceStorageDataLakeGen2FileSystemDelete,

		Importer: &schema.ResourceImporter{
			State: func(d *schema.ResourceData, meta interface{}) ([]*schema.ResourceData, error) {
				storageClients := meta.(*clients.Client).Storage
				ctx, cancel := context.WithTimeout(meta.(*clients.Client).StopContext, 5*time.Minute)
				defer cancel()

				id, err := filesystems.ParseResourceID(d.Id())
				if err != nil {
					return []*schema.ResourceData{d}, fmt.Errorf("Error parsing ID %q for import of Data Lake Gen2 File System: %v", d.Id(), err)
				}

				// we then need to look up the Storage Account ID
				account, err := storageClients.FindAccount(ctx, id.AccountName)
				if err != nil {
					return []*schema.ResourceData{d}, fmt.Errorf("Error retrieving Account %q for Data Lake Gen2 File System %q: %s", id.AccountName, id.DirectoryName, err)
				}
				if account == nil {
					return []*schema.ResourceData{d}, fmt.Errorf("Unable to locate Storage Account %q!", id.AccountName)
				}

				d.Set("storage_account_id", account.ID)

				return []*schema.ResourceData{d}, nil
			},
		},

		Timeouts: &schema.ResourceTimeout{
			Create: schema.DefaultTimeout(30 * time.Minute),
			Read:   schema.DefaultTimeout(5 * time.Minute),
			Update: schema.DefaultTimeout(30 * time.Minute),
			Delete: schema.DefaultTimeout(30 * time.Minute),
		},

		Schema: map[string]*schema.Schema{
			"name": {
				Type:         schema.TypeString,
				Required:     true,
				ForceNew:     true,
				ValidateFunc: validateStorageDataLakeGen2FileSystemName,
			},

			"storage_account_id": {
				Type:         schema.TypeString,
				Required:     true,
				ForceNew:     true,
				ValidateFunc: validate.StorageAccountID,
			},

			"properties": MetaDataSchema(),

			"ace": {
				Type:     schema.TypeSet,
				Optional: true,
				Computed: true,
				Elem: &schema.Resource{
					Schema: map[string]*schema.Schema{
						"scope": {
							Type:         schema.TypeString,
							Optional:     true,
							ValidateFunc: validation.StringInSlice([]string{"default", "access"}, false),
							Default:      "access",
						},
						"type": {
							Type:         schema.TypeString,
							Required:     true,
							ValidateFunc: validation.StringInSlice([]string{"user", "group", "mask", "other"}, false),
						},
						"id": {
							Type:         schema.TypeString,
							Optional:     true,
							ValidateFunc: validation.IsUUID,
						},
						"permissions": {
							Type:         schema.TypeString,
							Required:     true,
							ValidateFunc: validate.ADLSAccessControlPermissions,
						},
					},
				},
			},
		},
	}
}

func resourceStorageDataLakeGen2FileSystemCreate(d *schema.ResourceData, meta interface{}) error {
	accountsClient := meta.(*clients.Client).Storage.AccountsClient
	client := meta.(*clients.Client).Storage.FileSystemsClient
	pathClient := meta.(*clients.Client).Storage.ADLSGen2PathsClient
	ctx, cancel := timeouts.ForCreate(meta.(*clients.Client).StopContext, d)
	defer cancel()

	storageID, err := parse.StorageAccountID(d.Get("storage_account_id").(string))
	if err != nil {
		return err
	}

	aceRaw := d.Get("ace").(*schema.Set).List()
	acl, err := ExpandDataLakeGen2AceList(aceRaw)
	if err != nil {
		return fmt.Errorf("Error parsing ace list: %s", err)
	}

	// confirm the storage account exists, otherwise Data Plane API requests will fail
	storageAccount, err := accountsClient.GetProperties(ctx, storageID.ResourceGroup, storageID.Name, "")
	if err != nil {
		if utils.ResponseWasNotFound(storageAccount.Response) {
			return fmt.Errorf("Storage Account %q was not found in Resource Group %q!", storageID.Name, storageID.ResourceGroup)
		}

		return fmt.Errorf("Error checking for existence of Storage Account %q (Resource Group %q): %+v", storageID.Name, storageID.ResourceGroup, err)
	}

	fileSystemName := d.Get("name").(string)
	propertiesRaw := d.Get("properties").(map[string]interface{})
	properties := ExpandMetaData(propertiesRaw)

	id := client.GetResourceID(storageID.Name, fileSystemName)

	resp, err := client.GetProperties(ctx, storageID.Name, fileSystemName)
	if err != nil {
		if !utils.ResponseWasNotFound(resp.Response) {
			return fmt.Errorf("Error checking for existence of existing File System %q (Account %q): %+v", fileSystemName, storageID.Name, err)
		}
	}

	if !utils.ResponseWasNotFound(resp.Response) {
		return tf.ImportAsExistsError("azurerm_storage_data_lake_gen2_filesystem", id)
	}

	log.Printf("[INFO] Creating File System %q in Storage Account %q.", fileSystemName, storageID.Name)
	input := filesystems.CreateInput{
		Properties: properties,
	}
	if _, err := client.Create(ctx, storageID.Name, fileSystemName, input); err != nil {
		return fmt.Errorf("Error creating File System %q in Storage Account %q: %s", fileSystemName, storageID.Name, err)
	}

	if acl != nil {
		log.Printf("[INFO] Creating acl %q in File System %q in Storage Account %q.", acl, fileSystemName, storageID.Name)
		var aclString *string
		v := acl.String()
		aclString = &v
		accessControlInput := paths.SetAccessControlInput{
			ACL:   aclString,
			Owner: nil,
			Group: nil,
		}
		if _, err := pathClient.SetAccessControl(ctx, storageID.Name, fileSystemName, "/", accessControlInput); err != nil {
			return fmt.Errorf("Error setting access control for root path in File System %q in Storage Account %q: %s", fileSystemName, storageID.Name, err)
		}
	}

	d.SetId(id)
	return resourceStorageDataLakeGen2FileSystemRead(d, meta)
}

func resourceStorageDataLakeGen2FileSystemUpdate(d *schema.ResourceData, meta interface{}) error {
	accountsClient := meta.(*clients.Client).Storage.AccountsClient
	client := meta.(*clients.Client).Storage.FileSystemsClient
	pathClient := meta.(*clients.Client).Storage.ADLSGen2PathsClient
	ctx, cancel := timeouts.ForUpdate(meta.(*clients.Client).StopContext, d)
	defer cancel()

	id, err := filesystems.ParseResourceID(d.Id())
	if err != nil {
		return err
	}

	storageID, err := parse.StorageAccountID(d.Get("storage_account_id").(string))
	if err != nil {
		return err
	}

	aceRaw := d.Get("ace").(*schema.Set).List()
	acl, err := ExpandDataLakeGen2AceList(aceRaw)
	if err != nil {
		return fmt.Errorf("Error parsing ace list: %s", err)
	}

	// confirm the storage account exists, otherwise Data Plane API requests will fail
	storageAccount, err := accountsClient.GetProperties(ctx, storageID.ResourceGroup, storageID.Name, "")
	if err != nil {
		if utils.ResponseWasNotFound(storageAccount.Response) {
			return fmt.Errorf("Storage Account %q was not found in Resource Group %q!", storageID.Name, storageID.ResourceGroup)
		}

		return fmt.Errorf("Error checking for existence of Storage Account %q (Resource Group %q): %+v", storageID.Name, storageID.ResourceGroup, err)
	}

	propertiesRaw := d.Get("properties").(map[string]interface{})
	properties := ExpandMetaData(propertiesRaw)

	log.Printf("[INFO] Updating Properties for File System %q in Storage Account %q.", id.DirectoryName, id.AccountName)
	input := filesystems.SetPropertiesInput{
		Properties: properties,
	}
	if _, err = client.SetProperties(ctx, id.AccountName, id.DirectoryName, input); err != nil {
		return fmt.Errorf("Error updating Properties for File System %q in Storage Account %q: %s", id.DirectoryName, id.AccountName, err)
	}

	if acl != nil {
		log.Printf("[INFO] Creating acl %q in File System %q in Storage Account %q.", acl, id.DirectoryName, id.AccountName)
		var aclString *string
		v := acl.String()
		aclString = &v
		accessControlInput := paths.SetAccessControlInput{
			ACL:   aclString,
			Owner: nil,
			Group: nil,
		}
		if _, err := pathClient.SetAccessControl(ctx, id.AccountName, id.DirectoryName, "/", accessControlInput); err != nil {
			return fmt.Errorf("Error setting access control for root path in File System %q in Storage Account %q: %s", id.DirectoryName, id.AccountName, err)
		}
	}

	return resourceStorageDataLakeGen2FileSystemRead(d, meta)
}

func resourceStorageDataLakeGen2FileSystemRead(d *schema.ResourceData, meta interface{}) error {
	accountsClient := meta.(*clients.Client).Storage.AccountsClient
	client := meta.(*clients.Client).Storage.FileSystemsClient
	pathClient := meta.(*clients.Client).Storage.ADLSGen2PathsClient
	ctx, cancel := timeouts.ForRead(meta.(*clients.Client).StopContext, d)
	defer cancel()

	id, err := filesystems.ParseResourceID(d.Id())
	if err != nil {
		return err
	}

	storageID, err := parse.StorageAccountID(d.Get("storage_account_id").(string))
	if err != nil {
		return err
	}

	// confirm the storage account exists, otherwise Data Plane API requests will fail
	storageAccount, err := accountsClient.GetProperties(ctx, storageID.ResourceGroup, storageID.Name, "")
	if err != nil {
		if utils.ResponseWasNotFound(storageAccount.Response) {
			log.Printf("[INFO] Storage Account %q does not exist removing from state...", id.AccountName)
			d.SetId("")
			return nil
		}

		return fmt.Errorf("Error checking for existence of Storage Account %q for File System %q (Resource Group %q): %+v", storageID.Name, id.DirectoryName, storageID.ResourceGroup, err)
	}

	// TODO: what about when this has been removed?
	resp, err := client.GetProperties(ctx, id.AccountName, id.DirectoryName)
	if err != nil {
		if utils.ResponseWasNotFound(resp.Response) {
			log.Printf("[INFO] File System %q does not exist in Storage Account %q - removing from state...", id.DirectoryName, id.AccountName)
			d.SetId("")
			return nil
		}

		return fmt.Errorf("Error retrieving File System %q in Storage Account %q: %+v", id.DirectoryName, id.AccountName, err)
	}

	d.Set("name", id.DirectoryName)

	if err := d.Set("properties", resp.Properties); err != nil {
		return fmt.Errorf("Error setting `properties`: %+v", err)
	}

	// The above `getStatus` API request doesn't return the ACLs
	// Have to make a `getAccessControl` request, but that doesn't return all fields either!
	pathResponse, err := pathClient.GetProperties(ctx, id.AccountName, id.DirectoryName, "/", paths.GetPropertiesActionGetAccessControl)
	if err != nil {
		if utils.ResponseWasNotFound(pathResponse.Response) {
			log.Printf("[INFO] Root path does not exist in File System %q in Storage Account %q - removing from state...", id.DirectoryName, id.AccountName)
			d.SetId("")
			return nil
		}

		return fmt.Errorf("Error retrieving ACLs for Root path in File System %q in Storage Account %q: %+v", id.DirectoryName, id.AccountName, err)
	}

	acl, err := accesscontrol.ParseACL(pathResponse.ACL)
	if err != nil {
		return fmt.Errorf("Error parsing response ACL %q: %s", pathResponse.ACL, err)
	}
	d.Set("ace", FlattenDataLakeGen2AceList(acl))

	return nil
}

func resourceStorageDataLakeGen2FileSystemDelete(d *schema.ResourceData, meta interface{}) error {
	client := meta.(*clients.Client).Storage.FileSystemsClient
	ctx, cancel := timeouts.ForDelete(meta.(*clients.Client).StopContext, d)
	defer cancel()

	id, err := filesystems.ParseResourceID(d.Id())
	if err != nil {
		return err
	}

	resp, err := client.Delete(ctx, id.AccountName, id.DirectoryName)
	if err != nil {
		if !utils.ResponseWasNotFound(resp) {
			return fmt.Errorf("Error deleting File System %q in Storage Account %q: %+v", id.DirectoryName, id.AccountName, err)
		}
	}

	return nil
}

func validateStorageDataLakeGen2FileSystemName(v interface{}, k string) (warnings []string, errors []error) {
	value := v.(string)
	if !regexp.MustCompile(`^\$root$|^[0-9a-z-]+$`).MatchString(value) {
		errors = append(errors, fmt.Errorf(
			"only lowercase alphanumeric characters and hyphens allowed in %q: %q",
			k, value))
	}
	if len(value) < 3 || len(value) > 63 {
		errors = append(errors, fmt.Errorf(
			"%q must be between 3 and 63 characters: %q", k, value))
	}
	if regexp.MustCompile(`^-`).MatchString(value) {
		errors = append(errors, fmt.Errorf(
			"%q cannot begin with a hyphen: %q", k, value))
	}
	return warnings, errors
}
