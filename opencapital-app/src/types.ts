export type Org = {
  org_id: string;
  short_id: string;
  name: string;
  role: string;
  base_currency: string;
};

export type MeOrgs = {
  user_id: string;
  email: string;
  orgs: Org[];
};

export type KindeProfile = {
  id: string;
  preferred_email?: string;
  first_name?: string;
  last_name?: string;
  picture?: string;
};

export type VersionStatus = { version: string; validated: boolean };

export type CatalogEntry = {
  plugin_id: string;
  display_name: string;
  description: string;
  type: 'app' | 'datasource' | 'panel';
  required: boolean;
  installed: boolean;
  latest_validated_version?: string;
};

export type Catalog = {
  org_id: string;
  plugins: CatalogEntry[];
};
