export type KindeProfile = {
  id: string;
  preferred_email?: string;
  first_name?: string;
  last_name?: string;
  picture?: string;
};

export type SourceInfo = {
  url: string;
  publisher: string;
  verified: boolean;
};

export type CatalogEntry = {
  plugin_id: string;
  display_name: string;
  description: string;
  type: 'app' | 'datasource' | 'panel';
  required: boolean;
  installed: boolean;
  latest_validated_version?: string;
  source?: SourceInfo;
};

// A user-added plugin manifest URL (GET /v1/sources). The official set is not
// listed here — it comes from the curated plugins.json and shows as verified in
// the catalog.
export type PluginSource = {
  manifest_url: string;
  publisher: string;
  enabled: boolean;
};

export type Catalog = {
  plugins: CatalogEntry[];
};
