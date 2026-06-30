<?php

namespace App\Http\Controllers\Admin;

use App\Http\Controllers\Controller;
use App\Models\Tutorial;
use Illuminate\Http\Request;

class TutorialController extends Controller
{
    public function index()
    {
        return response()->json(['code' => 0, 'data' => Tutorial::orderBy('sort')->orderByDesc('id')->get()]);
    }

    public function store(Request $request)
    {
        $data = $request->validate([
            'title' => 'required|string|max:255',
            'category' => 'nullable|string|max:100',
            'content' => 'required|string',
            'sort' => 'nullable|integer',
            'enabled' => 'nullable|boolean',
        ]);

        $tutorial = Tutorial::create($data);
        return response()->json(['code' => 0, 'data' => $tutorial]);
    }

    public function update(Request $request, Tutorial $tutorial)
    {
        $data = $request->validate([
            'title' => 'sometimes|required|string|max:255',
            'category' => 'nullable|string|max:100',
            'content' => 'sometimes|required|string',
            'sort' => 'nullable|integer',
            'enabled' => 'nullable|boolean',
        ]);

        $tutorial->update($data);
        return response()->json(['code' => 0, 'data' => $tutorial]);
    }

    public function destroy(Tutorial $tutorial)
    {
        $tutorial->delete();
        return response()->json(['code' => 0, 'data' => ['message' => 'ok']]);
    }
}
