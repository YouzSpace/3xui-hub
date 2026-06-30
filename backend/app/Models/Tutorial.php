<?php

namespace App\Models;

use Illuminate\Database\Eloquent\Model;

class Tutorial extends Model
{
    protected $fillable = ['title', 'category', 'content', 'sort', 'enabled'];

    protected $casts = [
        'enabled' => 'boolean',
        'sort' => 'integer',
    ];
}
