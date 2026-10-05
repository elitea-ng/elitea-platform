export type JSONValue = null | boolean | number | string | JSONValue[] | { [key: string]: JSONValue };
export type PlatformExchange = (frame: Uint8Array) => Promise<Uint8Array>;
export class PlatformError extends Error { readonly code: string; }
export class SandboxClient {
 constructor(exchange: PlatformExchange, maxCalls?: number);
 call(operation: string, resource: { [key: string]: JSONValue }, arguments_?: { [key: string]: JSONValue }, payload?: Uint8Array): Promise<[JSONValue, Uint8Array]>;
 get_user_data(): Promise<JSONValue>;
 get_list_of_apps(cursor?: string | null, limit?: number): Promise<JSONValue>;
 get_app_details(application: string | number): Promise<JSONValue>;
 get_app_version_details(application: string | number, version: string | number): Promise<JSONValue>;
 get_mcp_toolkits(cursor?: string | null, limit?: number): Promise<JSONValue>;
 mcp_tool_call(toolkit: string | number, revision: string, tool: string, arguments_: { [key: string]: JSONValue }): Promise<JSONValue>;
 unsecret(name: string): Promise<JSONValue>;
 get_private_project_secret(name: string, defaultValue?: JSONValue): Promise<JSONValue>;
 bucket_exists(name: string): Promise<JSONValue>;
 create_bucket(name: string): Promise<JSONValue>;
 artifact(bucket: string): SandboxArtifact;
}
export class SandboxArtifact {
 list(prefix?: string, cursor?: string | null, limit?: number): Promise<JSONValue>;
 head(name: string): Promise<JSONValue>;
 get_content_bytes(name: string): Promise<Uint8Array>;
 create(name: string, content: string | Uint8Array): Promise<JSONValue>;
 append(name: string, text: string, expectedVersion: string): Promise<JSONValue>;
 delete(name: string, expectedVersion: string): Promise<JSONValue>;
}
