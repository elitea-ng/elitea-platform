// Image-owned schema gate. The native parent owns transport and authorization.
import {SandboxClient, boundedJSON} from "./platform_client.mjs";
import {retainedPipeExchange} from "./platform_pipe.mjs";
export function validateWorkspace(value) {
  const exact=(object,names)=>object && typeof object==="object" && !Array.isArray(object) && Object.keys(object).length===names.length && names.every(key=>Object.hasOwn(object,key));
  const hex=(value,lengths)=>typeof value==="string" && lengths.includes(value.length) && /^[a-f0-9]+$/.test(value);
  if (!exact(value,["revision","selection","manifest_sha256","policy_sha256"]) || value.revision!==1 || !hex(value.manifest_sha256,[64]) || !hex(value.policy_sha256,[64])) throw new Error("Invalid Code workspace binding");
  const s=value.selection;
  if (!exact(s,["toolkit_id","toolkit_reference_sha256","repository_id","commit","mode","include"]) || !Number.isSafeInteger(s.toolkit_id) || s.toolkit_id<1 || s.toolkit_id>2147483647 || !hex(s.toolkit_reference_sha256,[64]) || !hex(s.commit,[40,64]) || typeof s.repository_id!=="string" || !/^[\x21-\x7e]{1,128}$/.test(s.repository_id) || /[\/\\]/.test(s.repository_id) || !["read","readwrite"].includes(s.mode) || !Array.isArray(s.include) || s.include.length<1 || s.include.length>128) throw new Error("Invalid Code workspace selection");
  for (let i=0;i<s.include.length;i++) {
    const path=s.include[i];
    if (typeof path!=="string" || path.length<1 || path.length>1024) throw new Error("Invalid Code workspace path");
    const parts=path.split("/");
    if (parts.length>64 || parts.some(part=>!part || [".",".."].includes(part) || part.toLowerCase()===".git" || part.toLowerCase().startsWith(".elitea") || !/^[A-Za-z0-9._+@-]+$/.test(part)) || (i>0 && path<=s.include[i-1]) || s.include.slice(0,i).some(earlier=>path.startsWith(earlier+"/"))) throw new Error("Invalid Code workspace path");
  }
}
export function preparedCapabilities(request) {
  const hasBroker=Object.hasOwn(request,"platform_client"),hasWorkspace=Object.hasOwn(request,"workspace");
  if (hasWorkspace) validateWorkspace(request.workspace);
  if (hasBroker) {
    const value=request.platform_client;
    if (request.revision!==5 || !value || typeof value!=="object" || Array.isArray(value)
      || Object.keys(value).sort().join(",")!=="max_calls,max_total_bytes,policy_sha256,revision" || value.revision!==1
      || typeof value.policy_sha256!=="string" || !/^[a-f0-9]{64}$/.test(value.policy_sha256) || value.policy_sha256.length!==64
      || !Number.isSafeInteger(value.max_calls) || value.max_calls<1 || value.max_calls>4096
      || !Number.isSafeInteger(value.max_total_bytes) || value.max_total_bytes<1 || value.max_total_bytes>64*1024*1024) throw new Error("Invalid Code broker binding");
    return {baseRevision:Object.hasOwn(request,"native_dependencies")?3:Object.hasOwn(request,"dependency_bundle_sha256")?2:1,broker:value};
  }
  if (hasWorkspace) {
    if (request.revision!==4) throw new Error("Invalid Code workspace revision");
    return {baseRevision:Object.hasOwn(request,"native_dependencies")?3:Object.hasOwn(request,"dependency_bundle_sha256")?2:1,broker:null};
  }
  if (![1,2,3].includes(request.revision)) throw new Error("Invalid Code capability revision");
  return {baseRevision:request.revision,broker:null};
}
export function imageClient(binding) {
  if (!binding) return null;
  // Only fixed standard pipes are used. No HTTP, credentials, path or FD selectors.
  return new SandboxClient(retainedPipeExchange(Deno.stdin,Deno.stdout),binding.max_calls);
}
export async function writePlatformResult(result) {
  const bytes=boundedJSON({revision:1,result},256*1024);
  await Deno.writeFile("/workspace/.elitea-code-result",bytes,{createNew:true,mode:0o600});
}
