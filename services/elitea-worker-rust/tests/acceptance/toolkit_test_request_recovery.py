"""Headed browser acceptance against an isolated DCR-public OAuth fixture.

Create and remove one toolkit. Withhold one accepted response, then reject one
request before delivery. Require no repeated provider invocation.
"""
import argparse
import asyncio, json
from pathlib import Path
from playwright.async_api import async_playwright, expect
parser=argparse.ArgumentParser(description=__doc__)
parser.add_argument('--base-url', required=True)
parser.add_argument('--oauth-origin', required=True, help='Isolated local OAuth emulator with /stats')
parser.add_argument('--project-id', required=True, type=int)
parser.add_argument('--template-toolkit-id', required=True, type=int, help='Saved MCP toolkit for the DCR-public fixture')
parser.add_argument('--subject', required=True, help='Local test login subject')
parser.add_argument('--output-dir', required=True, type=Path)
args=parser.parse_args()
BASE=args.base_url.rstrip('/'); API=BASE+'/api/v2/elitea_core'
PROJECT=str(args.project_id); OUTPUT=args.output_dir
OUTPUT.mkdir(parents=True,exist_ok=True)

async def authorize(page):
    await page.get_by_role('button', name='Authorize', exact=True).wait_for(timeout=60000)
    await page.get_by_role('button', name='Authorize', exact=True).click()
    async with page.expect_popup() as opened:
        await page.get_by_role('dialog').get_by_role('button', name='Authorize', exact=True).click()
    popup=await opened.value
    await popup.get_by_role('button',name='Authorize test toolkit',exact=True).click(timeout=30000)
    await popup.wait_for_event('close',timeout=30000)
    await page.get_by_role('dialog').wait_for(state='hidden',timeout=30000)
    await page.get_by_role('combobox').wait_for(timeout=30000)

async def select_tool(page, marker):
    await page.get_by_role('combobox').click()
    await page.get_by_role('option',name='Echo marker',exact=True).click()
    await page.get_by_role('textbox',name='marker',exact=True).fill(marker)

async def count(page):
    response=await page.request.get(args.oauth_origin.rstrip('/')+'/stats')
    assert response.status==200
    return (await response.json())['dcr-public']['resource_calls']

async def receipt(page):
    values=await page.evaluate("Object.entries(sessionStorage).filter(([k]) => k.endsWith('toolkits.pendingTest')).map(([,v]) => JSON.parse(v))")
    assert len(values)==1, values
    value=values[0]
    assert set(value)=={'projectId','toolkitId','taskId','lookup'} and value['lookup']=='request', value
    return value

async def main():
    async with async_playwright() as p:
        browser=await p.chromium.launch(channel='chrome',headless=False)
        page=await browser.new_page(ignore_https_errors=True)
        toolkit=None; posts=[]; reads=[]; accepted=asyncio.Event(); release=asyncio.Event(); result={}
        page.on('request', lambda r: posts.append(r.url) if r.method=='POST' and '/test_tool/prompt_lib/' in r.url else reads.append(r.url) if r.method=='GET' and '/test_tool/prompt_lib/' in r.url else None)
        try:
            await page.goto(BASE+'/app/settings/create-personal-token')
            await page.get_by_role('textbox',name='Subject',exact=True).fill(args.subject)
            await page.get_by_role('button',name='Authorize',exact=True).click()
            await page.wait_for_url(BASE+'/app/**')
            r=await page.request.get(API+'/tool/prompt_lib/'+PROJECT+'/'+str(args.template_toolkit_id)); assert r.status==200
            source=await r.json()
            r=await page.request.post(API+'/tools/prompt_lib/'+PROJECT, data={'name':'rust-request-recovery-acceptance','type':source['type'],'settings':source['settings']})
            assert r.status in (200,201)
            toolkit=(await r.json())['id']
            print(json.dumps({'toolkit':toolkit,'browser':'fresh headed Chrome'}),flush=True)
            await page.goto(BASE+'/app/toolkits/all/'+str(toolkit))
            await authorize(page)
            marker='RUST_REQUEST_RECOVERY_BROWSER'
            await select_tool(page,marker)
            before=await count(page)
            async def lose_response(route):
                saved=await receipt(page)
                assert route.request.headers['idempotency-key']==saved['taskId']
                response=await route.fetch(timeout=120000)
                body=await response.json()
                assert response.status==200 and body.get('ok') is True and marker in json.dumps(body),body
                result.update({'execution_id':body['task_id'],'request_key':saved['taskId']})
                accepted.set()
                await release.wait()
                try: await route.abort('failed')
                except Exception: pass  # Reload can already cancel this deliberately withheld response.
            await page.route('**/test_tool/prompt_lib/**',lose_response,times=1)
            await page.get_by_role('button',name='RUN TOOL',exact=True).click()
            await asyncio.wait_for(accepted.wait(),120)
            saved=await receipt(page)
            assert marker not in json.dumps(saved)
            await page.reload(wait_until='domcontentloaded'); release.set()
            await expect(page.get_by_test_id('test-tool-result-payload')).to_contain_text(marker,timeout=60000)
            assert len(posts)==1,posts
            after=await count(page)
            assert after-before==1,(before,after)
            assert any('?lookup=request' in url for url in reads)
            stored=await page.evaluate("Object.keys(sessionStorage).filter(k => k.endsWith('toolkits.pendingTest'))")
            assert not stored,stored
            await page.screenshot(path=str(OUTPUT/'toolkit-request-recovery-success.png'),full_page=True)
            result.update({'accepted_response_lost':True,'browser_posts':len(posts),'provider_calls':after-before,'lookup_reads':len(reads),'marker_visible_after_reload':True})
            print(json.dumps(result),flush=True)

            # A request stopped before delivery must remain inconclusive and must not be retried.
            await authorize(page)
            await select_tool(page,'RUST_REQUEST_NOT_ADMITTED')
            async def never_deliver(route):
                await route.fulfill(status=503,content_type='application/json',body='{}')
            await page.route('**/test_tool/prompt_lib/**',never_deliver,times=1)
            await page.get_by_role('button',name='RUN TOOL',exact=True).click()
            await expect(page.get_by_test_id('test-tool-result')).to_have_attribute('data-status','unconfirmed',timeout=30000)
            unconfirmed=await receipt(page)
            await page.reload(wait_until='domcontentloaded')
            await expect(page.get_by_test_id('test-tool-result')).to_have_attribute('data-status','unconfirmed',timeout=30000)
            assert await receipt(page)==unconfirmed
            assert len(posts)==2,posts
            assert await count(page)==after
            result.update({'unaccepted_request_key':unconfirmed['taskId'],'unaccepted_lookup_retained':True,'no_automatic_resubmission':True})
            await page.screenshot(path=str(OUTPUT/'toolkit-request-recovery-unconfirmed.png'),full_page=True)
            (OUTPUT/'toolkit-request-recovery-evidence.json').write_text(json.dumps(result))
            print(json.dumps({'unaccepted_lookup_retained':True,'no_automatic_resubmission':True}),flush=True)
        finally:
            release.set()
            if toolkit:
                r=await page.request.delete(API+'/tool/prompt_lib/'+PROJECT+'/'+str(toolkit))
                print(json.dumps({'cleanup':r.status}),flush=True)
                assert r.status==204
            await browser.close()

asyncio.run(main())
