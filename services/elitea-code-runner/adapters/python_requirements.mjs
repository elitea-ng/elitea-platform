// Inspect Python syntax only. The preparation process never evaluates user code.
const MAX_SOURCE_BYTES = 256 * 1024;

export function discoverPythonRequirements(python, source) {
  if (
    typeof source !== "string" || source.includes("\0") ||
    new TextEncoder().encode(source).length > MAX_SOURCE_BYTES
  ) {
    throw new Error("Python dependency discovery requires bounded source text");
  }
  python.globals.set("_elitea_dependency_source", source);
  return JSON.parse(python.runPython(`
import ast as _dependency_ast
import json as _dependency_json
import importlib.util as _dependency_importlib
from pyodide.code import find_imports as _dependency_find_imports

def _discover_dependencies(source):
    tree = _dependency_ast.parse(source)
    nodes = []
    for node in _dependency_ast.walk(tree):
        if len(nodes) >= 40000:
            raise ValueError('Python dependency syntax exceeds its node limit')
        nodes.append(node)
    aliases, installers = set(), set()
    for node in nodes:
        if isinstance(node, _dependency_ast.Import):
            aliases.update(name.asname or name.name for name in node.names if name.name == 'micropip')
        elif isinstance(node, _dependency_ast.ImportFrom) and node.module == 'micropip':
            installers.update(name.asname or name.name for name in node.names if name.name == 'install')
    requirements = []
    dynamic = False
    for node in sorted((n for n in nodes if isinstance(n, _dependency_ast.Call)), key=lambda n: (n.lineno, n.col_offset)):
        function = node.func
        matches = (isinstance(function, _dependency_ast.Name) and function.id in installers)
        matches |= (isinstance(function, _dependency_ast.Attribute) and function.attr == 'install'
                    and isinstance(function.value, _dependency_ast.Name) and function.value.id in aliases)
        if not matches:
            continue
        argument = node.args[0] if len(node.args) == 1 else None
        if not node.args:
            argument = next((key.value for key in node.keywords if key.arg == 'requirements'), None)
        if any(key.arg != 'requirements' for key in node.keywords):
            raise ValueError('Package installation options require an explicit preparation contract')
        if isinstance(argument, _dependency_ast.Constant) and isinstance(argument.value, str):
            values = [argument.value]
        elif isinstance(argument, (_dependency_ast.List, _dependency_ast.Tuple)):
            if len(argument.elts) > 128:
                raise ValueError('Python dependency discovery exceeds 128 requirements')
            if all(isinstance(value, _dependency_ast.Constant) and isinstance(value.value, str) for value in argument.elts):
                values = [value.value for value in argument.elts]
            else:
                dynamic = True
                continue
        else:
            dynamic = True
            continue
        if any(not value or len(value) > 256 or '\\x00' in value for value in values):
            raise ValueError('Package requirements exceed their text limit')
        for value in values:
            if value not in requirements:
                requirements.append(value)
        if len(requirements) > 128:
            raise ValueError('Python dependency discovery exceeds 128 requirements')
    modules = _dependency_find_imports(source)
    automatic = not (aliases or installers)
    if automatic:
        import pyodide_js as _dependency_pyodide
        api = _dependency_pyodide._api
        mapping = api._import_name_to_package_name.to_py()
        for name in sorted(set(modules)):
            root = name.split('.')[0]
            if _dependency_importlib.find_spec(root) is None:
                package = mapping.get(root, root)
                if package not in requirements:
                    requirements.append(package)
        if len(requirements) > 128:
            raise ValueError('Python dependency discovery exceeds 128 requirements')
    return {'requirements': requirements, 'dynamic_installs': dynamic, 'automatic_imports': automatic}

_dependency_json.dumps(_discover_dependencies(_elitea_dependency_source), separators=(',', ':'))
`));
}
