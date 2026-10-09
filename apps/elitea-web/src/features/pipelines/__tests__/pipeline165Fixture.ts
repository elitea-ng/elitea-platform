/** The stored text of pipeline 165, version 191 (546 bytes, admitted and run by the Worker). */
export const PIPELINE_165_YAML = `state:
  input:
    type: str
  messages:
    type: list
  created:
    type: dict
  listing:
    type: dict
entry_point: mk
nodes:
  - id: mk
    type: toolkit
    toolkit_name: test
    tool: create_file
    input_mapping:
      filename:
        type: fixed
        value: verify-1ab920d-edge.txt
      filedata:
        type: fixed
        value: edge identity ok
    output: [created]
    transition: ls
  - id: ls
    type: toolkit
    toolkit_name: test
    tool: list_files
    input_mapping: {}
    output: [listing]
    transition: END
`;
